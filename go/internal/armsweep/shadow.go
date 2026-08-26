package armsweep

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// M-W2 gate (l) — the instance-kind gate (design/05 §5 B4b, changelog F-16).
//
// `excluded` keeps a shadow block out of the RESULT and out of the drift
// denominator. It does NOT keep it out of the indexes: idx_embedding_hnsw and
// both GIN indexes carry no partial predicate, and the ANN arm filters
// p_types_visible only AFTER the index walk. Every shadow block therefore
// spends HNSW scan budget and FTS bitmap on every PRODUCTION query — and
// catalog/insight blocks are aggregates, so they sit semantically central and
// are visited early. At 52 proxy blocks that is unmeasurable; at the ≈ 37 500
// catalog topics §6.2 computes it would be a permanent production regression
// introduced by the measurement programme itself.
//
// Hence: the shadow corpus is built in a copy restored from backups/*.dump, and
// the driver refuses to dump shadow types anywhere else.

// Instance kinds, as the settings key server.instance_kind carries them.
const (
	InstanceKindLive        = "live"
	InstanceKindMeasureCopy = "measure-copy"
)

// SettingInstanceKind is the key the gate reads. Deliberately a setting of the
// MEASURED instance and not an environment variable of the measuring host —
// the same doctrine PostStageState follows (client.go): an env var on this
// machine says nothing about the instance being measured. The key is
// restart-only server-side, so no hot settings write can relabel a live
// instance for the duration of one dump.
const SettingInstanceKind = "server.instance_kind"

// ErrNotMeasureCopy is the refusal class of gate (l). Its own error so a
// scheduler can tell "this run must not happen" from "this run failed".
var ErrNotMeasureCopy = errors.New("die Instanz ist keine Mess-Kopie")

// InstanceKind reads the instance's own provenance label. An instance that
// predates M-W2 has no such key and answers 404 — which this reports as an
// error, never as a pass: an instance that cannot say what it is has not said
// it is a measure copy.
func (c *Client) InstanceKind(ctx context.Context) (string, error) {
	v, err := c.Setting(ctx, SettingInstanceKind)
	if err != nil {
		return "", fmt.Errorf("Instanz-Art (%s): %w", SettingInstanceKind, err)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("Instanz-Art (%s): Wert ist %T, erwartet string", SettingInstanceKind, v)
	}
	return strings.TrimSpace(s), nil
}

// GateInstanceKind is the gate itself. It returns the instance kind it read —
// which the caller writes into the dump stamp — and refuses unless the instance
// says measure-copy.
//
// allowLive is the explicit override, and it is the same shape as
// --allow-outside-goldset: it buys passage, never a relabel. The returned kind
// stays the instance's own answer, so the stamp and the report keep saying
// which corpus the numbers came from.
//
// A dump without shadow types asks the instance nothing at all: the gate is
// about a claim only a shadow dump makes.
func GateInstanceKind(ctx context.Context, c *Client, shadowTypes []string, allowLive bool) (string, error) {
	if len(shadowTypes) == 0 {
		return "", nil
	}
	kind, err := c.InstanceKind(ctx)
	if err != nil {
		return "", err
	}
	if kind == InstanceKindMeasureCopy || allowLive {
		return kind, nil
	}
	return kind, fmt.Errorf("%w: %s=%q — ein Schatten-Dump gehört in eine wiederhergestellte Mess-Kopie (§5 B4b); Override: -allow-live-instance",
		ErrNotMeasureCopy, SettingInstanceKind, kind)
}
