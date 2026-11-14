#!/usr/bin/env python3
"""Assemble the settings page bundle from its sources (SETTINGS-PAGE §5.7).

``desktop/plugin.js`` is what Hermes Desktop loads: one uncompiled ES module that may import only
``@hermes/plugin-sdk`` and ``react``. It is generated from a head (imports, kit, registration) and the shared
core (``ui/core.js``, no module statements), and the generated file is committed, so users need neither Node
nor a build step. The output is deterministic: the same sources always give the same bytes.

Usage:
    python tools/assemble_ui.py            # write the bundle(s)
    python tools/assemble_ui.py --check    # exit 1 (naming the file) when a committed bundle is out of date

Exit status: 0 up to date / written, 1 out of date (``--check``), 2 a source is missing or invalid.
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

# output path -> sources in bundle order (all relative to the repository root)
TARGETS: dict[str, tuple[str, ...]] = {
    "desktop/plugin.js": ("ui/head.desktop.js", "ui/core.js"),
}
# The core is concatenated into an ES module now and into a classic script later: no module statements.
CORE_SOURCES = frozenset({"ui/core.js"})
_MODULE_STATEMENT = re.compile(r"^\s*(?:import|export)\b", re.MULTILINE)


class AssembleError(RuntimeError):
    pass


def _read(root: Path, rel: str) -> str:
    path = root / rel
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise AssembleError(f"{rel}: cannot read ({exc.strerror or exc})") from exc
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise AssembleError(f"{rel}: not UTF-8 ({exc})") from exc
    text = text.lstrip("﻿").replace("\r\n", "\n").replace("\r", "\n")
    return text.strip("\n") + "\n"


def _header(output: str, sources: tuple[str, ...]) -> str:
    lines = [
        f"// {output} — GENERATED FILE, do not edit by hand.",
        "// Built by tools/assemble_ui.py from:",
        *(f"//   {source}" for source in sources),
        "// Change the sources, then run: python tools/assemble_ui.py",
    ]
    return "\n".join(lines) + "\n"


def render(root: Path, output: str) -> str:
    """The exact bundle text for one target."""
    sources = TARGETS[output]
    parts = [_header(output, sources)]
    for source in sources:
        text = _read(root, source)
        if source in CORE_SOURCES:
            hit = _MODULE_STATEMENT.search(text)
            if hit:
                line = text.count("\n", 0, hit.start()) + 1
                raise AssembleError(f"{source}:{line}: the core must not contain import/export statements")
        parts.append(f"\n// ── {source} " + "─" * max(4, 96 - len(source)) + "\n\n" + text)
    return "".join(parts)


def build(root: Path = ROOT) -> dict[str, str]:
    """``{output path: bundle text}`` for every target."""
    return {output: render(root, output) for output in TARGETS}


def stale(root: Path = ROOT) -> list[str]:
    """Targets whose file on disk differs from what the sources produce (missing counts as differing)."""
    out = []
    for output, text in build(root).items():
        path = root / output
        try:
            current = path.read_bytes().decode("utf-8")
        except (OSError, UnicodeDecodeError):
            current = None
        if current != text:
            out.append(output)
    return out


def write(root: Path = ROOT) -> list[str]:
    """Write every target whose content changed; return the paths written."""
    written = []
    for output, text in build(root).items():
        path = root / output
        try:
            if path.read_bytes().decode("utf-8") == text:
                continue
        except (OSError, UnicodeDecodeError):
            pass
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(text.encode("utf-8"))
        written.append(output)
    return written


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--check", action="store_true", help="exit 1 when a committed bundle is out of date")
    parser.add_argument("--root", type=Path, default=ROOT, help=argparse.SUPPRESS)
    args = parser.parse_args(argv)
    root = args.root.resolve()
    try:
        if args.check:
            outdated = stale(root)
            for output in outdated:
                print(f"{output} is out of date with its sources; run: python tools/assemble_ui.py", file=sys.stderr)
            return 1 if outdated else 0
        for output in write(root):
            print(f"wrote {output}")
        return 0
    except AssembleError as exc:
        print(f"assemble_ui: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
