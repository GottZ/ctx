package goldbench

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/rrf"
)

// Achse rerank — mockt den LLM-Judge der query-rerank-Pipeline
// (internal/rrf/rerank.go, Rerank).

type rerankInput struct {
	Query string `json:"query"`
	Docs  []struct {
		Title    string `json:"title"`
		Category string `json:"category"`
		Content  string `json:"content"`
	} `json:"docs"`
}

type rerankGold struct {
	RelevantIndices []int `json:"relevant_indices"`
}

func axisRerank() axisDef {
	return axisDef{
		name: "rerank",
		build: func(c *Case) ([]ChatRequest, error) {
			var in rerankInput
			if err := decodeInto(c.Input, &in, "rerank input", c.ID); err != nil {
				return nil, err
			}
			docs := make([]rrf.SearchResult, 0, len(in.Docs))
			for _, d := range in.Docs {
				docs = append(docs, rrf.SearchResult{Title: d.Title, Category: d.Category, Content: d.Content})
			}
			// Cap wie Rerank (rerank.go:103-106): maximal RerankMaxDocs (15).
			if len(docs) > rrf.RerankMaxDocs {
				docs = docs[:rrf.RerankMaxDocs]
			}
			system, user := rrf.BenchBuildRerankJudgePrompt(in.Query, docs)
			return []ChatRequest{{
				System: system,
				User:   user,
				// Sampling wie llm.RerankOptions (client.go:396-405): 0 / 80.
				Opts: SamplingOpts{Temperature: 0, MaxTokens: 80},
			}}, nil
		},
		score: scoreRerank,
	}
}

// scoreRerank: nDCG@15 mit binärer Relevanz aus gold.relevant_indices
// (0-basiert), zusätzlich mean(score relevant) − mean(score irrelevant) auf
// der rohen 0-10-Skala. Parse mit dem Original-Parser inkl. Längen-Match
// (rerank.go:349).
func scoreRerank(runs []caseRun) (AxisResult, []CaseScore) {
	var ndcgs, separations []float64
	parsed := 0
	perCase := make([]CaseScore, 0, len(runs))
	for _, r := range runs {
		var in rerankInput
		_ = json.Unmarshal(r.c.Input, &in)
		var gold rerankGold
		_ = json.Unmarshal(r.c.Gold, &gold)

		docCount := len(in.Docs)
		if docCount > rrf.RerankMaxDocs {
			docCount = rrf.RerankMaxDocs
		}

		cs := CaseScore{ID: r.c.ID}
		scores, err := rrf.BenchParseRerankScores(firstOutput(r), docCount)
		if err != nil {
			ndcgs = append(ndcgs, 0)
			perCase = append(perCase, cs)
			continue
		}
		parsed++
		cs.Parsed = true
		cs.Score = ndcgBinary(scores, gold.RelevantIndices, rrf.RerankMaxDocs)
		ndcgs = append(ndcgs, cs.Score)
		if sep, ok := scoreSeparation(scores, gold.RelevantIndices); ok {
			separations = append(separations, sep)
		}
		perCase = append(perCase, cs)
	}
	return AxisResult{
		N:             len(runs),
		ParseRate:     ratioOrZero(parsed, len(runs)),
		PrimaryMetric: "ndcg_at_15",
		PrimaryScore:  meanOrZero(ndcgs),
		Secondary: map[string]float64{
			"mean_relevant_minus_irrelevant": meanOrZero(separations),
		},
	}, perCase
}

// scoreSeparation liefert mean(score relevant) − mean(score irrelevant);
// ok=false wenn eine der beiden Gruppen leer ist.
func scoreSeparation(scores []float64, relevant []int) (float64, bool) {
	rel := map[int]bool{}
	for _, i := range relevant {
		if i >= 0 && i < len(scores) {
			rel[i] = true
		}
	}
	var sumR, sumI float64
	var nR, nI int
	for i, s := range scores {
		if rel[i] {
			sumR += s
			nR++
		} else {
			sumI += s
			nI++
		}
	}
	if nR == 0 || nI == 0 {
		return 0, false
	}
	return sumR/float64(nR) - sumI/float64(nI), true
}

// Achse synthesis — mockt die query-synthesize-Pipeline
// (internal/llm/synthesize.go). Prompt-Bau über das exportierte
// llm.BuildPrompt (v5.2, ohne temporale Daten) — Quellen-Rendering und
// Nonce-Rule sind damit das Original.
//
// ABWEICHUNG (synthesis): Der H12-Budget-Pass (fitSourcesToBudget,
// synthesize.go:447) und die Auto-Window-Constraints entfallen — der Prompt
// geht ungekürzt raus; temporalDates ist nil (Gold-Cases ohne temporalen
// Kontext).

type synthesisInput struct {
	Question string `json:"question"`
	Sources  []struct {
		ID       int     `json:"id"`
		Title    string  `json:"title"`
		Category string  `json:"category"`
		Score    float64 `json:"score"`
		AgeDays  int     `json:"age_days"`
		Content  string  `json:"content"`
	} `json:"sources"`
}

type synthesisGold struct {
	Expect           string `json:"expect"` // "answer" | "refusal"
	AllowedCitations []int  `json:"allowed_citations"`
}

func axisSynthesis() axisDef {
	return axisDef{
		name: "synthesis",
		build: func(c *Case) ([]ChatRequest, error) {
			var in synthesisInput
			if err := decodeInto(c.Input, &in, "synthesis input", c.ID); err != nil {
				return nil, err
			}
			sources := make([]llm.Source, 0, len(in.Sources))
			for _, s := range in.Sources {
				sources = append(sources, llm.Source{
					ID:       strconv.Itoa(s.ID),
					Title:    s.Title,
					Category: s.Category,
					Content:  s.Content,
					Score:    s.Score,
					AgeDays:  s.AgeDays,
				})
			}
			system, user := llm.BuildPrompt(in.Question, sources, nil,
				llm.SynthesisSettings{PromptVersion: llm.PromptVersionV52})
			return []ChatRequest{{
				System: system,
				User:   user,
				// Sampling wie llm.SynthesisOptions (client.go:369-379):
				// 0.1 / NumPredict 500. repeat_penalty 1.1 entfällt
				// (OpenAI-API, siehe SamplingOpts-ABWEICHUNG).
				Opts: SamplingOpts{Temperature: 0.1, MaxTokens: 500},
			}}, nil
		},
		score: scoreSynthesis,
	}
}

// citationRe findet Inline-Zitate der Form [n].
var citationRe = regexp.MustCompile(`\[(\d+)\]`)

// synthRefusalText ist der FormatAnswer-Refusal-Text, deterministisch aus dem
// Original abgeleitet (synthesize.go:763-784 ersetzt den
// NO_RELEVANT_SOURCES-Sentinel durch den user-facing Text).
var synthRefusalText = llm.FormatAnswer(llm.NoRelevantResponse)

// scoreSynthesis setzt den Achsen-Vertrag um:
//   - gold.expect=="refusal": Output ist Refusal (Sentinel roh ODER
//     FormatAnswer-Refusal-Text) = 1;
//   - gold.expect=="answer": kein Refusal UND ≥1 Zitat [n] mit n in
//     allowed_citations UND kein Zitat außerhalb = 1.
//
// parse_rate = nicht-leerer Output (Freitext-Achse ohne Format-Contract).
func scoreSynthesis(runs []caseRun) (AxisResult, []CaseScore) {
	var scores []float64
	parsed := 0
	refusalCorrect, refusalTotal := 0, 0
	answerCorrect, answerTotal := 0, 0
	perCase := make([]CaseScore, 0, len(runs))
	for _, r := range runs {
		var gold synthesisGold
		_ = json.Unmarshal(r.c.Gold, &gold)

		cs := CaseScore{ID: r.c.ID}
		raw := strings.TrimSpace(firstOutput(r))
		if raw != "" {
			parsed++
			cs.Parsed = true
		}
		answer := llm.FormatAnswer(raw)
		isRefusal := raw == "" || strings.Contains(raw, llm.NoRelevantResponse) ||
			strings.HasPrefix(answer, synthRefusalText)

		switch gold.Expect {
		case "refusal":
			refusalTotal++
			// Leerer Output ist KEIN korrekter Refusal — nichts wurde geprüft.
			if raw != "" && isRefusal {
				refusalCorrect++
				cs.Score = 1
			}
		case "answer":
			answerTotal++
			if raw != "" && !isRefusal && citationsValid(answer, gold.AllowedCitations) {
				answerCorrect++
				cs.Score = 1
			}
		}
		scores = append(scores, cs.Score)
		perCase = append(perCase, cs)
	}
	return AxisResult{
		N:             len(runs),
		ParseRate:     ratioOrZero(parsed, len(runs)),
		PrimaryMetric: "contract_pass_rate",
		PrimaryScore:  meanOrZero(scores),
		Secondary: map[string]float64{
			"refusal_accuracy": ratioOrZero(refusalCorrect, refusalTotal),
			"answer_accuracy":  ratioOrZero(answerCorrect, answerTotal),
		},
	}, perCase
}

// citationsValid prüft: mindestens ein Zitat, jedes Zitat in allowed.
func citationsValid(answer string, allowed []int) bool {
	allowedSet := map[int]bool{}
	for _, a := range allowed {
		allowedSet[a] = true
	}
	matches := citationRe.FindAllStringSubmatch(answer, -1)
	if len(matches) == 0 {
		return false
	}
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil || !allowedSet[n] {
			return false
		}
	}
	return true
}

// Achse translate — mockt die query-translate-Pipeline
// (internal/llm/translate.go, TranslateQuery).

type translateInput struct {
	Query string `json:"query"`
}

type translateGold struct {
	MustContainTokens []string `json:"must_contain_tokens"`
	MaxLenRatio       float64  `json:"max_len_ratio"`
}

func axisTranslate() axisDef {
	return axisDef{
		name: "translate",
		build: func(c *Case) ([]ChatRequest, error) {
			var in translateInput
			if err := decodeInto(c.Input, &in, "translate input", c.ID); err != nil {
				return nil, err
			}
			return []ChatRequest{{
				System: llm.BenchTranslationSystemPrompt(),
				User:   in.Query, // roh, wie TranslateQuery (translate.go:113)
				// Sampling wie llm.TranslateOptions (client.go:383-392): 0 / 100.
				Opts: SamplingOpts{Temperature: 0, MaxTokens: 100},
			}}, nil
		},
		score: scoreTranslate,
	}
}

// scoreTranslate: Pass genau dann, wenn — nach dem Original-SQL-Strip
// (translate.go:126) — validateTranslation besteht UND alle
// gold.must_contain_tokens enthalten sind (case-insensitiv) UND der Output
// keine Umlaute trägt (Deutsch-Rest-Heuristik) UND len ≤ max_len_ratio ×
// Original (Default 3.0).
func scoreTranslate(runs []caseRun) (AxisResult, []CaseScore) {
	passCount, parsed := 0, 0
	var validateOK, tokensOK, umlautFree int
	perCase := make([]CaseScore, 0, len(runs))
	for _, r := range runs {
		var in translateInput
		_ = json.Unmarshal(r.c.Input, &in)
		var gold translateGold
		_ = json.Unmarshal(r.c.Gold, &gold)
		if gold.MaxLenRatio <= 0 {
			gold.MaxLenRatio = 3.0
		}

		cs := CaseScore{ID: r.c.ID}
		out := strings.TrimSpace(firstOutput(r))
		if out == "" {
			perCase = append(perCase, cs)
			continue
		}
		parsed++
		cs.Parsed = true
		translated := llm.BenchStripSQLMeta(out)

		valid := llm.BenchValidateTranslation(translated, in.Query)
		toks := containsAllTokens(translated, gold.MustContainTokens)
		noUmlaut := !containsUmlaut(translated)
		lenOK := float64(len(translated)) <= gold.MaxLenRatio*float64(len(in.Query))
		if valid {
			validateOK++
		}
		if toks {
			tokensOK++
		}
		if noUmlaut {
			umlautFree++
		}
		if valid && toks && noUmlaut && lenOK {
			passCount++
			cs.Score = 1
		}
		perCase = append(perCase, cs)
	}
	return AxisResult{
		N:             len(runs),
		ParseRate:     ratioOrZero(parsed, len(runs)),
		PrimaryMetric: "pass_rate",
		PrimaryScore:  ratioOrZero(passCount, len(runs)),
		Secondary: map[string]float64{
			"validate_rate":    ratioOrZero(validateOK, len(runs)),
			"token_rate":       ratioOrZero(tokensOK, len(runs)),
			"umlaut_free_rate": ratioOrZero(umlautFree, len(runs)),
		},
	}, perCase
}

// containsAllTokens prüft case-insensitiv auf alle Pflicht-Tokens.
func containsAllTokens(s string, tokens []string) bool {
	lower := strings.ToLower(s)
	for _, t := range tokens {
		if !strings.Contains(lower, strings.ToLower(t)) {
			return false
		}
	}
	return true
}

// containsUmlaut erkennt deutsche Umlaute + ß (Deutsch-Rest-Heuristik).
func containsUmlaut(s string) bool {
	return strings.ContainsAny(s, "äöüÄÖÜß")
}
