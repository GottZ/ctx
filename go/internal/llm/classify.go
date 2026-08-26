// Package llm — block sensitivity classification (G41 LLM audit).
//
// Two SEPARATE yes/no questions per block (user wording, masterplan G41):
// credentials and personal data. Answers are strict JSON booleans — there is
// deliberately NO confidence field (W18: Qwen self-reported confidence is
// uncalibrated, session-24 finding). A parse failure is NOT a verdict: the
// caller keeps the block at credentials and stamps a retry cooldown.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/promptguard"
	"github.com/GottZ/ctx/internal/redact"
	"github.com/GottZ/ctx/internal/util"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Audit questions, user wording verbatim (masterplan G41). Asked one call
// each — a combined question would let the model collapse two judgements
// into one token and hide which dimension fired.
const (
	//nolint:gosec // G101: the audit QUESTION about credentials, not a credential
	QuestionCredentials = "beinhaltet dieser block möglicherweise schützenswerte credentials?"
	QuestionPersonal    = "beinhaltet dieser block möglicherweise personenbezogene daten?"
)

// ClassifyTimeout bounds one audit question. Blocks are ~1-1.5k chars and the
// answer is a handful of tokens — generous for GPU, tight enough that a
// wedged backend cannot stall the batch for long.
const ClassifyTimeout = 60 * time.Second

// classifySystemPrompt forces the strict JSON-bool answer shape. German to
// match the question wording; the model judges content in any language.
const classifySystemPrompt = `Du bist ein Sicherheits-Klassifizierer für Wissensblöcke.
Du bekommst eine Ja/Nein-Frage und den Inhalt eines Blocks.
Antworte AUSSCHLIESSLICH mit einem JSON-Objekt der Form {"answer": true} oder {"answer": false}.
Kein anderer Text, keine Begründung, kein Markdown.
Im Zweifel antworte {"answer": true}.`

// ClassifyOptions returns deterministic sampling for the audit: the answer is
// one boolean, exploration adds nothing but variance. think comes from the
// pool row's model_map params, never from here.
func ClassifyOptions(numCtx int) Options {
	opts := Options{
		Temperature: 0,
		NumPredict:  32,
	}
	if numCtx > 0 {
		opts.NumCtx = numCtx
	}
	return opts
}

// ErrClassifyParse marks a response that reached us but is no verdict (not
// valid JSON / no boolean "answer"). The audit loop separates this (cooldown
// the BLOCK, continue) from chain failures (abort the RUN) via errors.Is.
var ErrClassifyParse = errors.New("llm: classify answer is no verdict")

// classifyAnswer is the strict wire shape. The pointer distinguishes a
// missing field from an explicit false — both exist in model output and only
// one is a verdict.
type classifyAnswer struct {
	Answer *bool `json:"answer"`
}

// ParseClassifyAnswer extracts the boolean verdict from one model response.
// Anything but a JSON object with a boolean "answer" field is a parse
// failure: no verdict, never a default.
func ParseClassifyAnswer(raw string) (bool, error) {
	var a classifyAnswer
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &a); err != nil {
		return false, fmt.Errorf("%w: invalid JSON: %w", ErrClassifyParse, err)
	}
	if a.Answer == nil {
		return false, fmt.Errorf("%w: no boolean \"answer\" field", ErrClassifyParse)
	}
	return *a.Answer, nil
}

// ClassifyContentLimit caps the block content that reaches the audit model, in
// RUNES (design 04 §4.5-a). 8 000 runes ≈ 2 000 tokens — far above the
// documented block size of 1-1.5k chars and far below the 50 KB write limit
// (internal/handler/context_store.go:247), which this path was the only one to
// pass on unbounded. At 1M blocks the audit is the busiest background prompt
// path in the system, so the cap is a cost lever as much as a hardening one.
//
// The cap is admissible ONLY because the deterministic detector reads the FULL
// content elsewhere: a truncation can hide a secret beyond rune 8 000, and the
// structural veto (design 04 §4.5-b, wave H9) is the condition under which
// this cap is allowed to exist, not an addition to it.
const ClassifyContentLimit = 8000

const (
	// classifyTruncSuffix keeps the truncation VISIBLE to the model — it must
	// know it is judging an excerpt, not a whole block. The same constant as
	// the synthesis path and promptguard.Assemble (M-W4a register).
	classifyTruncSuffix = redact.Truncated

	// classifySeparator divides the code-generated question from the guarded
	// block. It stays outside the marker: a payload can reproduce the byte
	// sequence, but a copy inside the block is data, not structure.
	classifySeparator = "\n\n---\n\n"

	// classifyMarkerID is the FIXED marker id — no nonce (design 04 §4.3):
	// exactly one foreign block, no boundary semantics for the model to
	// verify, so a nonce would buy nothing and cost fixture determinism. The
	// forgery it would defend against is closed structurally instead:
	// promptguard.Neutralize breaks a marker inside the payload, guessable id
	// or not.
	classifyMarkerID = "audit"

	// classifyMarkerKind is the rendered kind attribute of the marker.
	classifyMarkerKind = "block"

	// classifyTitleLabel keeps the German label of the pre-hardening prompt so
	// the wording the model was tuned on is unchanged; only its POSITION moves
	// (inside the block, where foreign text belongs).
	classifyTitleLabel = "Titel: "
)

// buildClassifyUser assembles the user message for one audit question.
//
// Shape: the code-generated question, the code separator, then EXACTLY ONE
// guarded block carrying every byte of foreign text — title and content alike.
// Before H8 this was a raw concat (design 04 §2.3-a): the separator
// "\n\n---\n\nTitel: " is trivially reproducible inside a 50 KB content, and a
// forged second section was indistinguishable from the real one to the model.
// It is not the separator that changed, it is WHERE the foreign text sits: the
// separator stays outside the marker, so a copy of it inside the payload is
// data instead of structure.
//
// Order is load-bearing twice over:
//
//   - truncate BEFORE the block is built, so the cap is measured against the
//     original text rather than against a CGJ-inflated one;
//   - Neutralize runs LAST over the assembled payload (inside Wrap), so no
//     later step can rejoin a broken control token across a cut or a suffix.
//
// No nonce and no Rule() sentence here: one foreign block, no boundary
// semantics for the model to verify (design 04 §4.3). Whether the three
// one-block pipelines get a nonce anyway is decision §8-E2, not this wave.
func buildClassifyUser(question, title, content string) string {
	body := classifyTitleLabel + title + "\n\n" +
		util.TruncateRunesWithSuffix(content, classifyTruncSuffix, ClassifyContentLimit)
	return question + classifySeparator +
		promptguard.Wrap(classifyMarkerID, classifyMarkerKind, body)
}

// ClassifyBlockBool asks ONE audit question about one block over the classify
// chain. required is ALWAYS credentials: the prompt carries unclassified
// block content, which is potential credentials by definition — no floor
// lookup, no trust shortcut. LocalOnly drops external rows even if a psql
// edit smuggled one past the 422 validation (defense in depth).
func ClassifyBlockBool(ctx context.Context, db *pgxpool.Pool, bpool *backends.Pool,
	question, title, content, blockID string, adm Admission,
) (bool, error) {
	user := buildClassifyUser(question, title, content)
	// TENANT-DECISION(classify-attribution): no APIKeyID set — the only caller
	// is the scheduler sensitivity-audit (events/scheduler.go:190), a background
	// job with no request-bound key. Leaving it "" keeps the row NULL, the same
	// background invariant as Dream/backfill (design/04 §4.3/§7 W3 lists classify
	// among the chain sites, but its sole caller is background, not foreground).
	resp, err := ChainCall{
		Pool:       bpool,
		Role:       backends.RoleClassify,
		Required:   backends.SensCredentials,
		LocalOnly:  true,
		Pipeline:   "sensitivity-audit",
		System:     classifySystemPrompt,
		User:       user,
		Opts:       ClassifyOptions(0),
		Format:     "json",
		DefTimeout: ClassifyTimeout,
		BlockIDs:   []string{blockID},
	}.Do(ctx, db, adm)
	if err != nil {
		return false, err
	}
	return ParseClassifyAnswer(resp.Message.Content)
}
