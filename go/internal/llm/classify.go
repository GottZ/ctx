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

// ClassifyBlockBool asks ONE audit question about one block over the classify
// chain. required is ALWAYS credentials: the prompt carries unclassified
// block content, which is potential credentials by definition — no floor
// lookup, no trust shortcut. LocalOnly drops external rows even if a psql
// edit smuggled one past the 422 validation (defense in depth).
func ClassifyBlockBool(ctx context.Context, db *pgxpool.Pool, bpool *backends.Pool, gaming backends.GamingState,
	question, title, content, blockID string,
) (bool, error) {
	user := question + "\n\n---\n\nTitel: " + title + "\n\n" + content
	resp, err := ChainCall{
		Pool:       bpool,
		Role:       backends.RoleClassify,
		Required:   backends.SensCredentials,
		Gaming:     gaming,
		LocalOnly:  true,
		Pipeline:   "sensitivity-audit",
		System:     classifySystemPrompt,
		User:       user,
		Opts:       ClassifyOptions(0),
		Format:     "json",
		DefTimeout: ClassifyTimeout,
		BlockIDs:   []string{blockID},
	}.Do(ctx, db)
	if err != nil {
		return false, err
	}
	return ParseClassifyAnswer(resp.Message.Content)
}
