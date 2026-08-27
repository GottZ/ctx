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

// InstanceKindUnknown is what a stamp carries when the instance answered the
// key but said nothing (wave X-W3a). Never the empty string: empty is reserved
// for a dry run and for every dump written before X-W3a, and a stamp field that
// means two things cannot be gated on. "unknown" compares unequal to both real
// kinds, so a campaign that mixes it in is refused rather than waved through.
const InstanceKindUnknown = "unknown"

// StampInstanceKind reads the label EVERY dump carries (wave X-W3a).
//
// Separate from the gate below because the two are different claims. The GATE
// is about where a shadow corpus may be built — a question only a shadow dump
// raises. The LABEL is about which instance a dump came from, and that is the
// F-32 campaign rule ("all dumps of one campaign come from ONE instance"),
// which is about ALL dumps. M-W2 built only the first, so the second was
// unguarded for exactly the artefacts a campaign is made of: X-W2b §4.2
// measured a compare over two measure-copy dumps and a LIVE noise pair running
// through with exit 0, because empty compares equal to empty.
//
// An instance that cannot be asked is an error, not a pass — the same shape
// MigrationsMax, PostStageState and EfSearchEffective already have in
// fillInstanceStamp: a dump whose provenance could not be read is not a dump
// with unknown provenance, it is a failed run.
func StampInstanceKind(ctx context.Context, c *Client) (string, error) {
	kind, err := c.InstanceKind(ctx)
	if err != nil {
		return "", err
	}
	if kind == "" {
		return InstanceKindUnknown, nil
	}
	return kind, nil
}

// CheckInstanceKind is the refusal half of gate (l), on a kind that has already
// been read.
//
// allowLive is the explicit override, and it is the same shape as
// --allow-outside-goldset: it buys passage, never a relabel. The caller keeps
// stamping the instance's own answer, so the stamp and the report keep saying
// which corpus the numbers came from.
func CheckInstanceKind(kind string, shadowTypes []string, allowLive bool) error {
	if len(shadowTypes) == 0 || kind == InstanceKindMeasureCopy || allowLive {
		return nil
	}
	return fmt.Errorf("%w: %s=%q — ein Schatten-Dump gehört in eine wiederhergestellte Mess-Kopie (§5 B4b); Override: -allow-live-instance",
		ErrNotMeasureCopy, SettingInstanceKind, kind)
}

// GateInstanceKind is the read-and-refuse form M-W2 shipped: it returns the
// instance kind it read and refuses unless the instance says measure-copy.
//
// A dump without shadow types asks the instance nothing HERE — the gate is
// about a claim only a shadow dump makes. Since X-W3a that no longer means an
// ordinary dump carries no kind: the driver reads the label through
// StampInstanceKind on every non-dry run and applies this refusal to it.
func GateInstanceKind(ctx context.Context, c *Client, shadowTypes []string, allowLive bool) (string, error) {
	if len(shadowTypes) == 0 {
		return "", nil
	}
	kind, err := c.InstanceKind(ctx)
	if err != nil {
		return "", err
	}
	return kind, CheckInstanceKind(kind, shadowTypes, allowLive)
}
