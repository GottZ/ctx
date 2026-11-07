package embed

// The differential of the strict decode (W02-09 hardening 6): for a valid
// backend answer it returns what the decode before it returned — every
// vector bit for bit, the token count — on both wires, with and without a
// Content-Length. The decode before it is the one the internal path keeps
// (embed.go: json.Decoder into the wire structs, then alignVectors resp.
// checkVectors), here over float32 as the gateway reads. A valid answer
// names every entry's index: an answer without one is the refusal E-2, not
// part of this corpus.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// diffNumber writes f in one of the forms backends print.
func diffNumber(f float64, form int) string {
	switch form {
	case 1:
		return strconv.FormatFloat(f, 'e', -1, 64)
	case 2:
		return strconv.FormatFloat(f, 'f', 20, 64)
	case 3:
		return strconv.FormatFloat(float64(float32(f)), 'g', -1, 32)
	case 4:
		return strings.ToUpper(strconv.FormatFloat(f, 'e', 6, 64))
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func diffVector(seed, dims, form int) string {
	parts := make([]string, dims)
	x := uint64(seed)*2654435761 + 12345
	for i := range parts {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		f := float64(x%2000003)/1000001.5 - 1.0
		switch {
		case i%11 == 3:
			f *= 1e-30
		case i%13 == 5:
			f = math.Copysign(0, -1)
		case i%17 == 7:
			f *= 1e30
		case i%19 == 9:
			f = float64(int(f * 10))
		}
		parts[i] = diffNumber(f, form)
		if i%23 == 11 {
			// Just below the float32 tie 1 + 3·2⁻²⁴: read at 32 bits it is
			// 1 + 2⁻²³; read at 64 bits first it is the tie, which rounds
			// to even, 1 + 2⁻²² — a decode that rounds twice differs.
			parts[i] = "1.0000001788139343261718749999"
		}
	}
	sep := ","
	if seed%3 == 0 {
		sep = ", "
	}
	return "[" + strings.Join(parts, sep) + "]"
}

// diffOpenAI is one OpenAI answer shape over n vectors.
func diffOpenAI(shape, n, dims, form int) string {
	entries := make([]string, n)
	for i := range entries {
		vec := diffVector(i+7*n+dims+form, dims, form)
		switch shape % 4 {
		case 0:
			entries[i] = fmt.Sprintf(`{"object":"embedding","index":%d,"embedding":%s}`, i, vec)
		case 1:
			entries[i] = fmt.Sprintf(`{"embedding":%s,"index":%d,"object":"embedding"}`, vec, i)
		case 2:
			entries[i] = fmt.Sprintf(`{ "INDEX" : %d , "Embedding" : %s , "x" : {"y":[1,"]",{"z":"\"}"}]}, "w": null }`, i, vec)
		case 3:
			entries[i] = fmt.Sprintf("{\n  \"\\u0069ndex\": %d,\n  \"embedding\": %s,\n  \"extra\": [[], {}, true, false, -1.5e3]\n}", i, vec)
		}
	}
	if shape%2 == 1 {
		for l, r := 0, len(entries)-1; l < r; l, r = l+1, r-1 {
			entries[l], entries[r] = entries[r], entries[l]
		}
	}
	data := "[" + strings.Join(entries, ",") + "]"
	switch shape / 4 {
	case 0:
		return `{"object":"list","data":` + data + `,"model":"m","usage":{"prompt_tokens":7,"total_tokens":7}}`
	case 1:
		return `{"usage":{"total_tokens":11,"z":[1]},"id":"e-\u00e9","data":` + data + "}\n"
	case 2:
		return `{"Data":` + data + `,"usage":null,"meta":{"a":"{[","b":[{}]}}  ` + "\n\t"
	case 3:
		return `{"\u0064ata":` + data + `,"usage":{"prompt_tokens":3}} trailing bytes the decoder never reads`
	}
	return `{"data":` + data + `}`
}

// diffOllama is one /api/embed answer shape over n vectors.
func diffOllama(shape, n, dims, form int) string {
	vecs := make([]string, n)
	for i := range vecs {
		vecs[i] = diffVector(i+5*n+dims+form, dims, form)
	}
	list := "[" + strings.Join(vecs, ",") + "]"
	switch shape {
	case 0:
		return `{"model":"m","embeddings":` + list + `,"prompt_eval_count":9}`
	case 1:
		return `{"total_duration":12,"Embeddings":` + list + `}`
	case 2:
		return "{\n \"embeddings\" : " + list + " ,\n \"PROMPT_EVAL_COUNT\" : 4 , \"x\":{\"]\":\"[\"} }\n"
	}
	return `{"\u0065mbeddings":` + list + `,"prompt_eval_count":null}`
}

// oldOpenAI and oldOllama are the decode before hardening 6.
func oldOpenAI(b []byte, n int) ([][]float32, openAIUsage, error) {
	var r openAIEmbedResponse[float32]
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(&r); err != nil {
		return nil, openAIUsage{}, err
	}
	vecs, err := alignVectors(r.Data, n, func(i int) (int, []float32) { return r.Data[i].Index, r.Data[i].Embedding })
	return vecs, r.Usage, err
}

func oldOllama(b []byte, n int) ([][]float32, int, error) {
	var r ollamaEmbedResponse[float32]
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(&r); err != nil {
		return nil, 0, err
	}
	return r.Embeddings, r.PromptEvalCount, checkVectors(r.Embeddings, n)
}

func sameVectors(a, b [][]float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if math.Float32bits(a[i][j]) != math.Float32bits(b[i][j]) {
				return false
			}
		}
	}
	return true
}

func TestEmbedRawDecodeDifferential(t *testing.T) {
	cases := 0
	for _, n := range []int{1, 2, 3} {
		for _, dims := range []int{1, 8, 1024} {
			for form := range 5 {
				for shape := range 16 {
					b := []byte(diffOpenAI(shape, n, dims, form))
					want, wu, werr := oldOpenAI(b, n)
					if werr != nil {
						t.Fatalf("openai shape %d n %d dims %d form %d: the old decode refuses the corpus answer: %v", shape, n, dims, form, werr)
					}
					for _, hint := range []int64{-1, int64(len(b))} {
						got, gu, err := decodeRawOpenAI[float32](&http.Response{ContentLength: hint}, bytes.NewReader(b), int64(len(b))+64, n, 0)
						cases++
						if err != nil || !sameVectors(got, want) || gu != wu {
							t.Errorf("openai shape %d n %d dims %d form %d hint %d: %v, usage %+v (old %+v), vectors equal %v", shape, n, dims, form, hint, err, gu, wu, sameVectors(got, want))
						}
					}
				}
				for shape := range 4 {
					b := []byte(diffOllama(shape, n, dims, form))
					want, wt, werr := oldOllama(b, n)
					if werr != nil {
						t.Fatalf("ollama shape %d n %d dims %d form %d: the old decode refuses the corpus answer: %v", shape, n, dims, form, werr)
					}
					for _, hint := range []int64{-1, int64(len(b))} {
						got, gt, err := decodeRawOllama[float32](&http.Response{ContentLength: hint}, bytes.NewReader(b), int64(len(b))+64, n, 0)
						cases++
						if err != nil || !sameVectors(got, want) || gt != wt {
							t.Errorf("ollama shape %d n %d dims %d form %d hint %d: %v, tokens %d (old %d), vectors equal %v", shape, n, dims, form, hint, err, gt, wt, sameVectors(got, want))
						}
					}
				}
			}
		}
	}
	t.Logf("%d decodes of valid answers equal to the decode before hardening 6", cases)
}
