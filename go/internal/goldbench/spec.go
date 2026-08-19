package goldbench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SpecConfig ist die strukturierte Spec-Provenienz eines Laufs (Drafter-
// Training design/04 §3.3). Befüllung per Flag -spec-config <json> durch den
// Treiber, der die Wahrheit kennt — bewusst KEINE Auto-Detektion aus Engine-
// APIs (/v1/models lügt). server_note bleibt daneben Freitext.
type SpecConfig struct {
	Algorithm          string `json:"algorithm"`                // "dspark" | "mtp" | "eagle" | "none"
	DrafterPath        string `json:"drafter_path,omitempty"`   // lokal lesbar ⇒ sha256 wird SELBST berechnet
	DrafterSHA256      string `json:"drafter_sha256,omitempty"` // sha256(model.safetensors) — Checkpoint-Identität
	DrafterSHAVerified bool   `json:"drafter_sha_verified"`     // true = von goldbench gehasht, nicht nur deklariert
	Gamma              int    `json:"gamma,omitempty"`          // block_size (SGLang) / num_speculative_tokens (vLLM)
	EngineBuild        string `json:"engine_build,omitempty"`   // Image-DIGEST, nie Tag (G0 verweigert Tag-only)
	TargetQuant        string `json:"target_quant,omitempty"`   // "nvfp4" | "bf16" | …
	KVCacheDtype       string `json:"kv_cache_dtype,omitempty"` // "fp8_e4m3" | "auto"
	TrainStep          string `json:"train_step,omitempty"`     // Checkpoint-Label der Trainings-Schleife
}

// ErrSpecProvenance meldet einen Provenienz-Konflikt: der deklarierte
// drafter_sha256 weicht vom selbst berechneten ab — der klassische Fehler
// einer automatisierten Schleife (kopierter Hash in einer Skript-Variablen,
// während Checkpoints durchgetauscht werden). Ein solcher Lauf ist UNGÜLTIG
// und startet gar nicht erst.
var ErrSpecProvenance = errors.New("goldbench: spec-config: Provenienz-Konflikt (drafter_sha256 deklariert ≠ berechnet)")

// ErrSpecConfig meldet ein unbrauchbares -spec-config.
var ErrSpecConfig = errors.New("goldbench: spec-config ungültig")

// drafterWeightsFile ist die Datei, deren sha256 die Checkpoint-Identität
// trägt (SpecForge-/HF-Layout: model.safetensors im Drafter-Verzeichnis).
const drafterWeightsFile = "model.safetensors"

// ParseSpecConfig dekodiert das -spec-config-JSON strikt (unbekannte Felder
// ⇒ Fehler, leer ⇒ Fehler, algorithm Pflicht).
func ParseSpecConfig(raw string) (*SpecConfig, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var sc SpecConfig
	if err := dec.Decode(&sc); err != nil {
		return nil, fmt.Errorf("%w: %w (erlaubt: algorithm, drafter_path, drafter_sha256, gamma, engine_build, target_quant, kv_cache_dtype, train_step)", ErrSpecConfig, err)
	}
	if sc.Algorithm == "" {
		return nil, fmt.Errorf("%w: algorithm fehlt", ErrSpecConfig)
	}
	if sc.DrafterSHAVerified {
		// Verifiziert wird nur, was goldbench selbst hasht — ein deklariertes
		// true wäre genau die Selbstbescheinigung, die der Stempel ausschließt.
		return nil, fmt.Errorf("%w: drafter_sha_verified darf nicht deklariert werden", ErrSpecConfig)
	}
	return &sc, nil
}

// ResolveDrafterSHA verifiziert die Drafter-Provenienz: ist drafter_path lokal
// lesbar, wird sha256(model.safetensors) berechnet — weicht ein deklarierter
// Hash ab ⇒ ErrSpecProvenance; stimmt er oder fehlt er ⇒ Wert übernommen,
// DrafterSHAVerified=true. Ist der Pfad nicht lesbar (Remote-Lauf), gilt die
// Deklaration mit DrafterSHAVerified=false (P1 verlangt für Promote den
// verifizierten Modus).
func ResolveDrafterSHA(sc *SpecConfig) error {
	if sc == nil || sc.DrafterPath == "" {
		return nil
	}
	p := sc.DrafterPath
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		p = filepath.Join(p, drafterWeightsFile)
	}
	f, err := os.Open(p)
	if err != nil {
		sc.DrafterSHAVerified = false
		return nil
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("goldbench: spec-config: hash %s: %w", p, err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if sc.DrafterSHA256 != "" && !strings.EqualFold(sc.DrafterSHA256, sum) {
		return fmt.Errorf("%w: deklariert %s, berechnet %s (%s)", ErrSpecProvenance, sc.DrafterSHA256, sum, p)
	}
	sc.DrafterSHA256 = sum
	sc.DrafterSHAVerified = true
	return nil
}
