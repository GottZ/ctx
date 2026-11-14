"""The plugin's workflows, and what the README and the manifest say about what they test.

Three files of the plugin live outside its directory: the two workflow files and the banner. The workflow has
to run when any of them changes (a test of the plugin guards the upstream workflow file), the private-value
scan has to cover each of them, and the scan has to run again when a pull request's text is edited. Where the
README or the manifest describes testing (main does not block, Linux only, when the self-check runs), it says
what the code and the workflows do.
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


def _readme() -> str:
    return (ROOT / "README.md").read_text(encoding="utf-8")


def _manifest() -> dict:
    return _load_yaml((ROOT / "plugin.yaml").read_text(encoding="utf-8"))


def test_the_readme_says_main_does_not_block_where_the_workflow_lets_it_pass():
    contract = _workflow()["jobs"]["contract"]
    assert "matrix.ref == 'main'" in str(contract.get("continue-on-error"))
    row = next(line for line in _readme().splitlines() if line.startswith("| `main` |"))
    assert "does not block" in row and "daily run" in row, row


def test_platforms_without_a_test_run_are_named_in_the_readme():
    """The manifest lists macOS and Windows; every job of the plugin's workflows runs on Linux."""
    repo = _repo()
    runners = set()
    for name in ("hermes-ctx-discord-policy.yml", "hermes-ctx-discord-policy-upstream.yml"):
        jobs = _load_yaml((repo / ".github" / "workflows" / name).read_text(encoding="utf-8"))["jobs"]
        runners |= {job["runs-on"] for job in jobs.values()}
    assert runners == {"ubuntu-latest"}, runners
    untested = {"macos": "macOS", "windows": "Windows"}
    listed = [untested[p] for p in _manifest()["platforms"] if p in untested]
    limits = _readme().split("What does not work, or only partly:", 1)[1].split("\n## ", 1)[0]
    assert listed and all(name in limits for name in listed), listed


def test_the_self_check_and_the_fail_closed_claims_say_when_they_hold():
    """The form of each attachment point is probed when Hermes loads the plugin and when the bot connects (not
    "at start"); nothing of the plugin runs under host isolation, so nothing fails closed there."""
    manifest = _manifest()["description"]
    for text in (" ".join(_readme().split()), manifest):     # README lines wrap anywhere
        assert "when it starts" not in text and "checked at start" not in text
        assert "when the Discord bot connects" in text
    assert "while it runs, protection fails closed" in manifest
    how = _readme().split("## How it works", 1)[1].split("\n## ", 1)[0]
    assert "Under `plugins.isolation: host` none of this applies" in how


def test_setting_descriptions_name_what_else_acts_on_them():
    schema = _manifest()["config_schema"]
    assert "template" in schema["frame_prompt"]["description"]          # a server's frame template replaces it
    assert "model" in schema["notify_operators"]["description"]         # the model notice is a direct message too
