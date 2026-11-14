"""The plugin's own workflow runs for every file of the plugin and scans each of them for private values.

Three files of the plugin live outside its directory: the two workflow files and the banner. The workflow has
to run when any of them changes (a test of the plugin guards the upstream workflow file), the private-value
scan has to cover each of them, and the scan has to run again when a pull request's text is edited.
"""

from __future__ import annotations

import shlex
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
PLUGIN_GLOB = "integrations/hermes/plugin/ctx_discord_policy/**"
OUTSIDE = {
    ".github/workflows/hermes-ctx-discord-policy.yml",
    ".github/workflows/hermes-ctx-discord-policy-upstream.yml",
    "integrations/hermes/assets/ctx_discord_policy.webp",
}


def _load_yaml(text: str):
    try:
        import yaml  # PyYAML

        return yaml.safe_load(text)
    except ImportError:
        from ruamel.yaml import YAML

        return YAML(typ="safe").load(text)


def _repo() -> Path:
    for base in ROOT.parents:
        if (base / ".github" / "workflows" / "hermes-ctx-discord-policy.yml").is_file():
            return base
    raise AssertionError(f".github/workflows/hermes-ctx-discord-policy.yml not found above {ROOT}")


def _workflow() -> dict:
    data = _load_yaml((_repo() / ".github" / "workflows" / "hermes-ctx-discord-policy.yml").read_text(encoding="utf-8"))
    data["on"] = data.pop("on", None) or data.pop(True)  # YAML 1.1 reads the key `on` as true
    return data


def test_the_outside_files_exist():
    repo = _repo()
    assert all((repo / path).is_file() for path in OUTSIDE), [p for p in OUTSIDE if not (repo / p).is_file()]


def test_the_workflow_runs_for_every_file_of_the_plugin():
    triggers = _workflow()["on"]
    for event in ("push", "pull_request"):
        assert set(triggers[event]["paths"]) == {PLUGIN_GLOB} | OUTSIDE, event
    assert set(triggers["pull_request"]["types"]) == {"opened", "synchronize", "reopened", "edited"}


def test_the_scan_covers_every_file_of_the_plugin():
    steps = _workflow()["jobs"]["scan"]["steps"]
    run = next(step["run"] for step in steps if "ci/private_scan.py" in step.get("run", ""))
    argv = shlex.split(run)
    also = {argv[i + 1] for i, arg in enumerate(argv) if arg == "--also"}
    assert also == OUTSIDE, also
    assert "--text-env" in argv and "PR_TEXT" in argv
