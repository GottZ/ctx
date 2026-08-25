package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// CanonicalWrite is the canonical form of a staged LLM-path write (F6-C6
// D-W2). Its canonical JSON serialization is BOTH the server-held payload
// stored in context_pending_writes (089) AND the sole input of PayloadHash —
// the confirm call selects by hash and can alter nothing.
//
// Canonicalization contract (flat canonics per rejected finding D1-m3):
//   - Field order is fixed by this struct's declaration order (encoding/json
//     emits struct fields in declaration order — deterministic).
//   - Tags are SORTED (a copy; plan §payload_hash "tags(sorted)"). Tag order
//     is not semantic for a block, so the staged execute writes the sorted set.
//   - Metadata map keys are sorted by encoding/json itself (maps marshal in
//     sorted key order, recursively) — no hand-rolled walker needed.
//   - The hash BINDS the resolved post-gate sensitivity (value + manual +
//     detector): a confirm can never execute under a different classification
//     than the card promised.
//
// JSONB lesson (D-W1, pending_write_integration_test.go): the hash is formed
// over THIS client-side canonical serialization, NEVER over bytes read back
// from the JSONB column — JSONB normalizes key order and whitespace, so a
// rehash of the stored column would not reproduce the staged hash.
type CanonicalWrite struct {
	Op       string `json:"op"`           // 'store' | 'update'
	ID       string `json:"id,omitempty"` // update only (D-W6+): target block
	Scope    string `json:"scope"`        // resolved write scope (post scope-gate)
	Category string `json:"category"`
	Title    string `json:"title"`
	Content  string `json:"content"`
	// Tags are normalized to sorted order by Canonical(); keep the field
	// private to normalization — construct via struct literal, hash via
	// Canonical()/PayloadHash.
	Tags     []string       `json:"tags,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	// Resolved post-gate sensitivity intent (mirrors store.SensitivityWrite —
	// all three components feed UpsertBlock and are therefore hash-bound).
	Sensitivity       string `json:"sensitivity"`
	SensitivityManual bool   `json:"sensitivity_manual"`
	SensitivityDetect bool   `json:"sensitivity_detector"`
	Type              string `json:"type,omitempty"` // explicit block type ('' = auto-classify)

	// Update form only (op 'update', D-W6a) — both omitempty, so every
	// existing op 'store' hash stays byte-identical.
	//
	// UpdateFields is the authoritative SET of fields this update writes
	// (sorted by Canonical(), like Tags). It disambiguates "clear
	// tags/metadata" (field listed, value empty — omitempty drops the empty
	// value from the JSON) from "leave unchanged" (field not listed). The
	// confirm execute rebuilds UpdateBlockData from THIS list, never from
	// value presence.
	UpdateFields []string `json:"update_fields,omitempty"`
	// BaseUpdatedAt pins the block state the staged card was rendered against
	// (TOCTOU guard, D1-M3): context_blocks.updated_at at stage time,
	// UTC/RFC3339Nano. The confirm rejects — without consuming the token —
	// when the block's updated_at no longer matches (lost-update protection).
	BaseUpdatedAt string `json:"base_updated_at,omitempty"`

	// Blob form only (op OpBlobStore, W02-8) — all three omitempty and
	// appended last, so every existing 'store'/'update' canonicalization keeps
	// its exact bytes and every payload_hash a client already holds stays
	// confirmable.
	//
	// Data is the DECODED payload; encoding/json renders a []byte as base64,
	// which makes the canonical form deterministic without a second encoding
	// decision here. The hash therefore binds the payload itself — a confirm
	// can no more alter the bytes than it can alter a block's content.
	Filename string `json:"filename,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Data     []byte `json:"data,omitempty"`
}

// OpBlobStore is the third staged op (W02-8), beside 'store' and 'update'.
// context_pending_writes.op is a free TEXT column, so the value needs no
// migration; what it does need is to be spelled in ONE place, because the
// stage site, the tamper check and the execute arm all compare against it.
const OpBlobStore = "blob_store"

// Canonical returns the canonical JSON bytes of the write: fixed field order,
// sorted tags and update_fields (input slices untouched), metadata keys
// sorted by encoding/json. These bytes are what gets staged as the
// server-held payload.
func (c CanonicalWrite) Canonical() ([]byte, error) {
	if len(c.Tags) > 0 {
		sorted := make([]string, len(c.Tags))
		copy(sorted, c.Tags)
		sort.Strings(sorted)
		c.Tags = sorted
	}
	if len(c.UpdateFields) > 0 {
		sorted := make([]string, len(c.UpdateFields))
		copy(sorted, c.UpdateFields)
		sort.Strings(sorted)
		c.UpdateFields = sorted
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("canonical write: marshal: %w", err)
	}
	return b, nil
}

// PayloadHash returns sha256(canonical) as lowercase hex plus the canonical
// bytes themselves, so stage sites hash and store the SAME serialization in
// one call (never hash one thing and store another).
func (c CanonicalWrite) PayloadHash() (hash string, canonical []byte, err error) {
	canonical, err = c.Canonical()
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), canonical, nil
}
