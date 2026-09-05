// ctx project init — the provisioning path (workflow I-I, design/02 §4.6). init
// resolves the repo identity, calls the server-admin `project-provision` compound
// (tenant + scope + owner key + project row + repo-agent key + quota, ONE tx),
// then stores the minted REPO-AGENT key (K12 template: home=<scope>, allowed=[],
// write=[]) 0600 UNDER ~/.config/ctx/projects/ — NEVER in the working directory
// (a secret in the CWD is a commit hazard, §4.6 step 4). The .ctx-project marker
// keeps its W5 `identity=` line (additive-only: scope + key-path pointers are
// added as comments the detector ignores).
//
// Idempotent: a second `ctx project init` for the same repo is a No-op — the
// server returns provisioned=false with the existing project (the reveal-once
// keys were shown ONCE at first provision), and init writes NO new key file.
package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/GottZ/ctx/internal/clientconfig"
)

// provisionResult mirrors the flat handleProjectProvision response. The two key
// plaintexts are populated ONLY on a fresh provision (provisioned=true).
type provisionResult struct {
	Success        bool       `json:"success"`
	Provisioned    bool       `json:"provisioned"`
	Scope          string     `json:"scope"`
	RepoID         string     `json:"repo_id"`
	Project        projectRow `json:"project"`
	OwnerKeyID     string     `json:"owner_key_id"`
	OwnerKey       string     `json:"owner_key"`
	AgentKeyID     string     `json:"agent_key_id"`
	AgentKey       string     `json:"agent_key"`
	AgentHomeScope string     `json:"agent_home_scope"`
	TokenSet       bool       `json:"token_set"`
}

// projectKeyDir is the directory holding per-project repo-agent keys, UNDER the
// ctx config dir (never the CWD). Created 0700 on first write.
func projectKeyDir() string {
	return filepath.Join(clientconfig.BaseDir(), "projects")
}

// projectKeyPath maps an identity to its stable 0600 key-file path. The filename
// is a hash of the identity (opaque, filesystem-safe — the identity may contain
// '/' or ':'); the identity↔tenant truth lives server-side (§4.6 step 2), the
// filename need not be reversible.
func projectKeyPath(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(projectKeyDir(), hex.EncodeToString(sum[:])[:32])
}

// writeAgentKeyFile persists the repo-agent key to its 0600 file under
// ~/.config/ctx/projects/ (mkdir 0700). The file is a config-format snippet so
// `ctx --as-project` (a later baustein) can load it directly: CTX_KEY + the
// identity/scope provenance as comments. It NEVER lands in the working directory.
func writeAgentKeyFile(identity, baseURL, agentKey, scope string) (string, error) {
	dir := projectKeyDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	path := projectKeyPath(identity)
	var b strings.Builder
	b.WriteString("# ctx repo-agent key (project " + identity + ")\n")
	b.WriteString("# minted to the K12 agent-key template: home=" + scope + ", allowed=[], write=[]\n")
	b.WriteString("# stored 0600 here, NEVER in the repo working directory (design/02 §4.6)\n")
	if baseURL != "" {
		b.WriteString("CTX_BASE_URL=" + baseURL + "\n")
	}
	b.WriteString("CTX_KEY=" + agentKey + "\n")
	// 0600 written directly (O_CREATE|O_TRUNC honours the mode only on create; chmod
	// makes an overwrite of an existing 0644 file tight too).
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("chmod %s: %w", path, err)
	}
	return path, nil
}

// writeProvisionMarker writes the .ctx-project marker for a provisioned repo:
// the W5 `identity=` line (unchanged, so detect keeps working) PLUS scope +
// key-path pointers as COMMENTS (additive — the detector ignores '#' lines and
// only reads `identity=`). Never a secret.
func writeProvisionMarker(dir, identity, scope, keyPath string) error {
	content := "# ctx project identity — a detect shortcut, re-verified against git on read\n" +
		"# (design/03 §4.3). NOT a trust anchor: for github:/git-root: identities detect\n" +
		"# honors this file ONLY when it matches independent git detection; a mismatch\n" +
		"# forces confirmation on a TTY and errors when piped.\n" +
		"identity=" + identity + "\n" +
		"# scope=" + scope + "  (the tenant scope this repo's issue corpus lives in)\n" +
		"# repo-agent key: " + keyPath + " (0600, NOT here — never commit a key)\n"
	//nolint:gosec // G306: .ctx-project holds only the (non-secret) identity + pointers and is meant to be committed — world-readable 0644 is correct, unlike the 0600 key file
	return os.WriteFile(filepath.Join(dir, ctxProjectFile), []byte(content), 0o644)
}

// runProjectProvisionInit is the I-I init flow: resolve identity → provision →
// store the repo-agent key 0600 → write the .ctx-project marker. chosen is the
// already-resolved identity (flag / --repo / detection, from runProjectInit).
func runProjectProvisionInit(c *Client, baseURL string, chosen resolvedIdentity) error {
	resp, _, err := c.Do(http.MethodPost, "/api/manage",
		map[string]any{"action": "project-provision", "data": map[string]any{"identity": chosen.Identity}})
	if err != nil {
		return err
	}
	if err := checkSettingsEnvelope(resp); err != nil {
		return err
	}
	var res provisionResult
	if err := json.Unmarshal(resp, &res); err != nil {
		PrintJSON(resp)
		return err
	}

	// Idempotent re-run: No-op with the existing project (no new key file).
	if !res.Provisioned {
		if !StdoutIsTTY() {
			PrintJSON(resp)
			return nil
		}
		fmt.Printf("already provisioned (no-op):\n")
		printProjectDetail(res.Project)
		return nil
	}

	// Fresh provision: store the repo-agent key 0600 under ~/.config, write the marker.
	keyPath, kerr := writeAgentKeyFile(chosen.Identity, baseURL, res.AgentKey, res.Scope)
	if kerr != nil {
		// The project IS provisioned server-side; surface the key so it is not lost.
		Errorf("warning: could not write the agent key file: %v", kerr)
	}
	if werr := writeProvisionMarker(markerDir(), chosen.Identity, res.Scope, keyPath); werr != nil {
		Errorf("warning: could not write %s: %v", ctxProjectFile, werr)
	}

	if !StdoutIsTTY() {
		PrintJSON(resp)
		return nil
	}
	fmt.Printf("provisioned %s\n", chosen.Identity)
	fmt.Printf("  tenant scope:   %s\n", res.Scope)
	fmt.Printf("  project id:     %s\n", res.RepoID)
	if keyPath != "" {
		fmt.Printf("  repo-agent key: stored 0600 at %s\n", keyPath)
	}
	fmt.Printf("\n  owner key (SHOWN ONCE — store it, it is your tenant-admin credential):\n    %s\n", res.OwnerKey)
	if keyPath == "" {
		fmt.Printf("\n  repo-agent key (SHOWN ONCE — the key file could not be written):\n    %s\n", res.AgentKey)
	}
	return nil
}

// markerDir resolves the git repo root for the .ctx-project marker (or the CWD
// when not in a repo) — the marker is committed, the key never is.
func markerDir() string {
	if root, ok := gitOutput(".", "rev-parse", "--show-toplevel"); ok && root != "" {
		return root
	}
	return "."
}
