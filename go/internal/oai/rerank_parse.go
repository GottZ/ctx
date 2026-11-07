package oai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/promptguard"
)

// The request parse of POST /v1/rerank and POST /v2/rerank (design/02 §4.9;
// wave W02-10), built like the embeddings parse (parseEmbedTop): every
// top-level key is a CTX key (model, query, documents, top_n,
// return_documents, max_tokens_per_doc) or REJECT (400
// unsupported_parameter) — the route sends nothing of the client's request
// but the query and the documents. A repeated key is 400 invalid_json (the
// checked value is the sent value), a key that differs from a known one only
// in letter case is an unknown key. A document is a string or an object
// whose one key is text (Jina's form); both are scored alike.
//
// The parse holds on the heap only what the request's reservation covers
// (the ledger of FOLGE-E at this route): no key is decoded into a string
// beyond the longest name (keyIndex), a refusal names a key cut to
// echoNameMax bytes (decodeKey), the model name is decoded only up to
// modelRawMax raw bytes (a longer one cannot name an alias — probeNoAlias),
// and the model, the query and the documents are sized before they are
// decoded — their exact decoded length (decodedLen) plus rerankDocSlotBytes
// per document are booked through cover, then each is written into one
// allocation of that size. A refusal of the budget is errParseOverloaded: 503
// server_overloaded before the allocation.

// rerankDocSlotBytes is what the parse allocates per document beside its
// text: the string header in the documents slice (16 bytes), the header of
// the text as it goes upstream (16 bytes, rerankRequest.wire — a prefix of
// the document when max_tokens_per_doc cuts it, never a copy) and its rune
// count (8 bytes, rerankRequest.runes). Booked with the texts, before the
// three slices are made.
const rerankDocSlotBytes = 16 + 16 + 8

// rerankKeys are the CTX keys of the top level; rerankDocKeys the keys of a
// document object.
var (
	rerankKeys = map[string]bool{
		"model": true, "query": true, "documents": true,
		"top_n": true, "return_documents": true, "max_tokens_per_doc": true,
	}
	rerankDocKeys = map[string]bool{"text": true}
)

// rerankNames/rerankIndex and rerankDocNames/rerankDocIndex are the named
// keys of each level in sort order and their index (levelKeys).
var (
	rerankNames, rerankIndex       = levelNames(slices.Collect(maps.Keys(rerankKeys)))
	rerankDocNames, rerankDocIndex = levelNames(slices.Collect(maps.Keys(rerankDocKeys)))
)

// rerankWindow and rerankDocWindow are how many members the parse reads of
// a level: one more than its names. Past them the level holds an unknown or
// a repeated key among them, so the answer is a 400 from them alone and the
// rest is never read (the embeddings parse's window, hardening H-1 of
// W02-09).
var (
	rerankWindow    = len(rerankNames) + 1
	rerankDocWindow = len(rerankDocNames) + 1
)

// rerankRequest is the parsed client request.
type rerankRequest struct {
	model string
	// modelOverlong: the raw model value is past modelRawMax — not
	// decoded, model stays empty and the route resolves no alias for it.
	modelOverlong bool
	query         string
	// docs are the documents as sent (decoded): the echo of
	// return_documents.
	docs []string
	// wire are the documents as they go upstream: docs[i], or its prefix of
	// max_tokens_per_doc × 9/5 runes (promptguard.RuneBudget).
	wire []string
	// runes are the runes of wire[i]; queryRunes the query's.
	runes      []int
	queryRunes int
	// topN is the client's top_n, 0 = all results.
	topN int
	// returnDocs is the client's return_documents; set reports that it sent
	// one (else the dialect's default).
	returnDocs, returnDocsSet bool
	// maxTokensPerDoc is the client's max_tokens_per_doc, 0 = not sent.
	maxTokensPerDoc int
	// queryRaw and docsRaw are the values as sent, for the row's body policy.
	queryRaw, docsRaw json.RawMessage
	// runesSum is Σ over the documents of (query + document) runes as they
	// go upstream — the meter's InputRunes of a rerank request (keyquota:
	// every pair is the engine's work); bytesSum the same in bytes.
	runesSum, bytesSum int
}

// parseRerankTop is the alias-independent half of the parse: JSON, the
// class of every key (unknown ⇒ 400 before the model is resolved), the
// model, the query, the shape of documents and the types of top_n,
// return_documents and max_tokens_per_doc. The alias's max_documents
// follows once it is known (checkRerankPolicy).
//
// The order of the answers: no valid JSON object ⇒ invalid_json; a repeated
// key ⇒ invalid_json (the first repeat in body order); else an unknown key ⇒
// unsupported_parameter, the smallest in sort order; then the values in the
// order model, query, documents, top_n, return_documents,
// max_tokens_per_doc. cover books the decoded texts before they are
// allocated; nil books nothing (the parse outside a request).
func parseRerankTop(body []byte, cover func(int64) bool) (*rerankRequest, *paramError) {
	it, isObject := newMemberIter(body)
	if !isObject || !json.Valid(body) {
		return nil, notAnObject()
	}
	var vals [maxLevelKeys][]byte
	var keys levelKeys
	for range rerankWindow {
		k, v, more := it.next()
		if !more {
			break
		}
		idx := keyIndex(k, rerankIndex)
		if keys.add(k, idx) {
			return nil, repeated("", decodeKey(k))
		}
		if idx < 0 {
			keys.refuse(k)
			continue
		}
		vals[idx] = v
	}
	if keys.reject != nil {
		return nil, unsupportedParameter(decodeKey(keys.reject))
	}
	req := &rerankRequest{}
	for _, step := range []func() *paramError{
		func() *paramError { return req.readModel(vals[rerankIndex["model"]], cover) },
		func() *paramError { return req.readQuery(vals[rerankIndex["query"]], cover) },
		func() *paramError { return req.readDocuments(vals[rerankIndex["documents"]], cover) },
		func() *paramError { return readPositive(vals[rerankIndex["top_n"]], "top_n", &req.topN) },
		func() *paramError { return req.readReturnDocuments(vals[rerankIndex["return_documents"]]) },
		func() *paramError {
			return readPositive(vals[rerankIndex["max_tokens_per_doc"]], "max_tokens_per_doc", &req.maxTokensPerDoc)
		},
	} {
		if perr := step(); perr != nil {
			return nil, perr
		}
	}
	req.cut()
	return req, nil
}

// readModel reads model as the embeddings parse does, its decode sized and
// booked first (decodeText).
func (req *rerankRequest) readModel(v json.RawMessage, cover func(int64) bool) *paramError {
	if v == nil || isNull(v) {
		return missingParameter("model")
	}
	raw, ok := stringContent(v)
	if !ok {
		return invalidType("model", "a string")
	}
	// Past modelRawMax raw bytes no alias name can stand (FOLGE-P hardening
	// 5, B-1): not decoded.
	if len(raw) > modelRawMax {
		req.modelOverlong = true
		return nil
	}
	s, ok := decodeText(raw, cover)
	if !ok {
		return errParseOverloaded
	}
	if s == "" {
		return missingParameter("model")
	}
	req.model = s
	return nil
}

// readQuery reads query: a non-empty string, sized, booked and decoded into
// one allocation.
func (req *rerankRequest) readQuery(v json.RawMessage, cover func(int64) bool) *paramError {
	if v == nil || isNull(v) {
		return missingParameter("query")
	}
	raw, ok := stringContent(v)
	if !ok {
		return invalidType("query", "a string")
	}
	s, ok := decodeText(raw, cover)
	if !ok {
		return errParseOverloaded
	}
	if s == "" {
		return unsupportedValue("query", "the query must not be empty")
	}
	req.query, req.queryRaw = s, v
	return nil
}

// decodeText books the decoded length of the raw JSON string content raw
// through cover (nil: books nothing) and decodes it into one allocation of
// that length — the string json.Unmarshal gives it (writeDecoded); false
// when the budget refused.
func decodeText(raw []byte, cover func(int64) bool) (string, bool) {
	size := decodedLen(raw)
	if cover != nil && !cover(int64(size)) {
		return "", false
	}
	var b strings.Builder
	b.Grow(size)
	writeDecoded(&b, raw)
	return b.String(), true
}

// readDocuments reads documents: an array of strings or of objects with the
// one key text (Jina's form), at least one, at most the code ceiling of
// rerank.max_documents — counted before one element is decoded; the alias's
// own max_documents follows once it is resolved (checkRerankPolicy). The
// texts are sized (the size run stops at the first element the decode
// refuses), booked with the slots of all n documents, then decoded into one
// allocation of the booked size.
func (req *rerankRequest) readDocuments(v json.RawMessage, cover func(int64) bool) *paramError {
	if v == nil || isNull(v) {
		return missingParameter("documents")
	}
	if kindOf(v) != kindArray {
		return invalidType("documents", "an array of strings or of objects with text")
	}
	n, over := arrayElems(v, modelalias.CeilingRerankMaxDocuments)
	switch {
	case over:
		return unsupportedValue("documents", fmt.Sprintf("more documents than any model accepts (%d)", modelalias.CeilingRerankMaxDocuments))
	case n == 0:
		return unsupportedValue("documents", "at least one document is required")
	}
	total := 0
	eachArrayElem(v, func(i int, e []byte) bool {
		raw, perr := docText(i, e)
		if perr != nil {
			return false
		}
		total += decodedLen(raw)
		return true
	})
	if cover != nil && !cover(int64(total)+int64(n)*rerankDocSlotBytes) {
		return errParseOverloaded
	}
	req.docs = make([]string, 0, n)
	var texts strings.Builder
	texts.Grow(total)
	var perr *paramError
	eachArrayElem(v, func(i int, e []byte) bool {
		var raw []byte
		if raw, perr = docText(i, e); perr != nil {
			return false
		}
		from := texts.Len()
		writeDecoded(&texts, raw)
		req.docs = append(req.docs, texts.String()[from:]) // the builder never grows past the booked size: no copy
		return true
	})
	if perr != nil {
		return perr
	}
	req.docsRaw = v
	return nil
}

// docText is the raw string content of document i: the element itself, or
// the text of an object element — whose keys are checked as a level of their
// own (the window, repeats, unknown keys named at documents[i].<key>). The
// param path is built on a refusal only: the size run and the decode call
// it for every element.
func docText(i int, e []byte) ([]byte, *paramError) {
	switch kindOf(e) {
	case kindString:
		raw, _ := stringContent(e)
		return raw, nil
	case kindObject:
	default:
		return nil, invalidType(docPath(i), "a string or an object with text")
	}
	it, _ := newMemberIter(e)
	var keys levelKeys
	var text []byte
	for range rerankDocWindow {
		k, val, more := it.next()
		if !more {
			break
		}
		idx := keyIndex(k, rerankDocIndex)
		if keys.add(k, idx) {
			return nil, repeated(docPath(i), decodeKey(k))
		}
		if idx < 0 {
			keys.refuse(k)
			continue
		}
		text = val
	}
	if keys.reject != nil {
		return nil, unsupportedParameter(keyPath(docPath(i), decodeKey(keys.reject)))
	}
	if text == nil || isNull(text) {
		return nil, missingParameter(keyPath(docPath(i), "text"))
	}
	raw, ok := stringContent(text)
	if !ok {
		return nil, invalidType(keyPath(docPath(i), "text"), "a string")
	}
	return raw, nil
}

// docPath is the param of document i.
func docPath(i int) string { return fmt.Sprintf("documents[%d]", i) }

// readPositive reads an optional positive integer into dst (absent or null:
// dst stays 0). A raw value longer than jsonIntMax overflows an int and is
// refused without being decoded.
func readPositive(v json.RawMessage, param string, dst *int) *paramError {
	if v == nil || isNull(v) {
		return nil
	}
	if !hasKind(v, kindInteger) {
		return invalidType(param, "an integer")
	}
	if len(bytes.TrimSpace(v)) > jsonIntMax {
		return unsupportedValue(param, "must be a positive integer")
	}
	var n int
	if err := json.Unmarshal(v, &n); err != nil || n < 1 {
		return unsupportedValue(param, "must be a positive integer")
	}
	*dst = n
	return nil
}

func (req *rerankRequest) readReturnDocuments(v json.RawMessage) *paramError {
	if v == nil || isNull(v) {
		return nil
	}
	switch string(bytes.TrimSpace(v)) {
	case "true":
		req.returnDocs = true
	case "false":
		req.returnDocs = false
	default:
		return invalidType("return_documents", "a boolean")
	}
	req.returnDocsSet = true
	return nil
}

// cut sets the documents as they go upstream (max_tokens_per_doc: each cut
// to its first rerankDocRunes runes — a prefix, no copy), their runes and the
// meter's sums. A document within the limit goes as sent.
func (req *rerankRequest) cut() {
	limit := rerankDocRunes(req.maxTokensPerDoc)
	req.wire = make([]string, len(req.docs))
	req.runes = make([]int, len(req.docs))
	req.queryRunes = utf8.RuneCountInString(req.query)
	for i, d := range req.docs {
		req.wire[i], req.runes[i] = runePrefix(d, limit)
		req.runesSum = usageTotal(req.runesSum, usageTotal(req.queryRunes, req.runes[i]))
		req.bytesSum = usageTotal(req.bytesSum, usageTotal(len(req.query), len(req.wire[i])))
	}
}

// rerankDocRunes is the rune limit of a document under max_tokens_per_doc
// tokens (design/02 §4.9): the 9/5 rune budget of promptguard.RuneBudget —
// the measured densest tokenization, so the cut keeps at most what the
// tokens can hold. 0 (not sent) and a count whose budget overflows an int
// cut nothing.
func rerankDocRunes(tokens int) int {
	if tokens <= 0 || tokens > math.MaxInt/9 {
		return math.MaxInt
	}
	return promptguard.RuneBudget(tokens)
}

// runePrefix is the prefix of s of at most limit runes and its rune count —
// counted on s itself (FOLGE-P runeCount: no copy), stopping at the limit.
func runePrefix(s string, limit int) (string, int) {
	n := 0
	for i := 0; i < len(s); n++ {
		if n == limit {
			return s[:i], n
		}
		if s[i] < utf8.RuneSelf {
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s, n
}

// rerankLimits are the alias's rerank policy as the route reads it
// (design/02 §3.2): unset keys at their defaults, a tenant alias clamped to
// gateway.tenant_alias_ceiling.rerank_max_documents.
type rerankLimits struct {
	maxDocuments int
	subBatch     int
}

func rerankLimitsOf(a modelalias.Alias, s Settings) rerankLimits {
	p := a.Policy
	l := rerankLimits{
		maxDocuments: optOr(p.RerankMaxDocuments, modelalias.DefaultRerankMaxDocuments),
		subBatch:     optOr(p.RerankSubBatch, modelalias.DefaultRerankSubBatch),
	}
	if tenantAlias(a) {
		l.maxDocuments = min(l.maxDocuments, max(s.TenantAliasCeiling.RerankMaxDocuments, 0))
	}
	return l
}

// checkRerankPolicy is the alias-dependent half of the parse: the number of
// documents.
func checkRerankPolicy(req *rerankRequest, l rerankLimits) *paramError {
	if n := len(req.docs); n > l.maxDocuments {
		return unsupportedValue("documents", fmt.Sprintf("at most %d documents per request", l.maxDocuments))
	}
	return nil
}
