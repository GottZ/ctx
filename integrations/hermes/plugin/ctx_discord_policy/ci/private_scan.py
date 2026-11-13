#!/usr/bin/env python3
"""Scan the repository tree and commit messages for private values that must never be published.

The private values are stored only as keyed HMAC-SHA256 digests (``ci/private_patterns.txt``); the key
lives outside the repository (CI secret ``DSP_PRIVATE_SCAN_KEY``, locally ``--key-file``). Without the key
the digests cannot be brute-forced or used as a confirmation oracle, and without the key the scan refuses
to run (exit 2) instead of passing silently.

Matching works on normalised text, so separators and casing do not hide a value:
- words (>= 4 characters) are looked for as substrings of each whitespace-separated chunk, lower-cased
  with everything except letters and digits removed (catches CamelCase, concatenations, digit suffixes,
  ``a-b_c`` compounds, words inside URLs and paths); shorter words only as whole tokens;
- Discord snowflakes as substrings of digit runs with ``_``/``,``/``'`` separators removed;
- dates (``YYYY-MM-DD``) and UUIDs as tokens; ``/opt`` paths as tokens and prefixes (the squashed form
  is also covered by the word rule).
Generic, non-secret rules (internal host paths, any UUID) are plain regular expressions.

Usage:
    DSP_PRIVATE_SCAN_KEY=... python ci/private_scan.py            # tracked files + every commit message
    python ci/private_scan.py --key-file KEY --no-commits
    python ci/private_scan.py --key-file KEY --hash KIND VALUE     # pattern line for a new private value

Exit status: 0 clean, 1 findings, 2 usage/environment error (missing key, git failure, bad pattern file).
"""

from __future__ import annotations

import argparse
import hmac
import hashlib
import os
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
PATTERNS = Path(__file__).with_name("private_patterns.txt")
REPO_NAME = "ctx"
KINDS = ("word", "snowflake", "uuid", "path", "date")
MIN_SUBSTRING_WORD = 4

UUID_RE = re.compile(r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}")
TOKEN_RE = re.compile(r"[A-Za-z0-9]+")
DIGIT_RUN_RE = re.compile(r"\d(?:[\d_,']*\d)?")
DATE_RE = re.compile(r"\d{4}-\d{2}-\d{2}")
PATH_RE = re.compile(r"/+opt(?:/+[A-Za-z0-9_.-]+)*")
LINE_RE = re.compile(r"^(word|snowflake|uuid|path|date) (\d{1,3}) ([0-9a-f]{64})$")

GENERIC_RULES = (
    ("internal-path", re.compile(r"(?<![A-Za-z0-9_])/(?:compose|root)/[A-Za-z0-9_.-]+")),
    ("internal-path", re.compile(r"\.gotmp\b")),
    ("uuid", UUID_RE),
)
# The scanner spells its own plain generic rules; keyed rules still apply to it.
GENERIC_EXEMPT = {"ci/private_scan.py"}
BINARY_SUFFIXES = {".png", ".jpg", ".jpeg", ".gif", ".ico", ".webp", ".zip", ".gz", ".whl", ".pdf"}


class ScanError(RuntimeError):
    pass


def normalise(kind: str, value: str) -> str:
    if kind == "word":
        return re.sub(r"[^a-z0-9]", "", value.lower())
    if kind == "snowflake":
        return re.sub(r"\D", "", value)
    if kind == "uuid":
        return value.lower()
    if kind == "path":
        return re.sub(r"/+", "/", value).rstrip("/.")
    return value


def digest(key: bytes, kind: str, value: str) -> str:
    return hmac.new(key, f"{kind}:{normalise(kind, value)}".encode("utf-8"), hashlib.sha256).hexdigest()


class Patterns:
    def __init__(self, by_kind: dict[str, set[str]], word_lengths: set[int], snowflake_lengths: set[int],
                 count: int) -> None:
        self.by_kind = by_kind
        self.word_lengths = word_lengths
        self.snowflake_lengths = snowflake_lengths
        self.count = count


def load_patterns(path: Path = PATTERNS) -> Patterns:
    by_kind: dict[str, set[str]] = {k: set() for k in KINDS}
    word_lengths: set[int] = set()
    snowflake_lengths: set[int] = set()
    count = 0
    for number, raw in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        if not raw.strip() or raw.startswith("#"):
            continue
        match = LINE_RE.match(raw)
        if not match:
            raise ScanError(f"{path.name}:{number}: malformed pattern line")
        kind, length, value = match.group(1), int(match.group(2)), match.group(3)
        by_kind[kind].add(value)
        if kind == "word":
            word_lengths.add(length)
        elif kind == "snowflake":
            snowflake_lengths.add(length)
        count += 1
    return Patterns(by_kind, word_lengths, snowflake_lengths, count)


def _owner_contexts(root: Path) -> tuple[re.Pattern[str], ...]:
    """The repository owner may appear only inside this repository's own GitHub slug."""
    try:
        manifest = (root / "plugin.yaml").read_text(encoding="utf-8")
    except OSError:
        return ()
    match = re.search(rf"^homepage:\s*https://github\.com/([A-Za-z0-9-]+)/{REPO_NAME}\s*$", manifest, re.M)
    if not match:
        return ()
    owner = re.escape(match.group(1))
    return (re.compile(rf"(?<=github\.com/){owner}(?=/{REPO_NAME}\b)", re.IGNORECASE),
            re.compile(rf"\b{owner}(?=/{REPO_NAME}\b)", re.IGNORECASE))


def _mask(text: str, contexts) -> str:
    for rule in contexts:
        text = rule.sub(lambda m: " " * len(m.group(0)), text)
    return text


def _line_of(text: str, offset: int) -> int:
    return text.count("\n", 0, max(offset, 0)) + 1


def scan_text(label: str, text: str, key: bytes, patterns: Patterns, *, contexts=(), generic: bool = True) -> list[str]:
    findings: set[str] = set()
    masked = _mask(text, contexts)

    def hit(kind: str, offset: int) -> None:
        findings.add(f"{label}:{_line_of(masked, offset)}: private {kind} value (keyed match)")

    words = patterns.by_kind["word"]
    lengths = sorted(n for n in patterns.word_lengths if n >= MIN_SUBSTRING_WORD)
    if words and lengths:
        # Per whitespace-separated chunk: separators inside a chunk (-, _, ., /, case changes, digits) do
        # not hide a word; a word split by whitespace is two words.
        for chunk in re.finditer(r"\S+", masked):
            flat = re.sub(r"[^a-z0-9]", "", chunk.group(0).lower())
            for length in lengths:
                for start in range(0, len(flat) - length + 1):
                    if digest(key, "word", flat[start:start + length]) in words:
                        hit("word", chunk.start())
        for match in TOKEN_RE.finditer(masked):
            token = match.group(0).lower()
            if len(token) < MIN_SUBSTRING_WORD and digest(key, "word", token) in words:
                hit("word", match.start())
    snowflakes = patterns.by_kind["snowflake"]
    if snowflakes:
        for match in DIGIT_RUN_RE.finditer(masked):
            digits = re.sub(r"\D", "", match.group(0))
            for length in patterns.snowflake_lengths:
                for start in range(0, len(digits) - length + 1):
                    if digest(key, "snowflake", digits[start:start + length]) in snowflakes:
                        hit("snowflake", match.start())
    for kind, regex in (("uuid", UUID_RE), ("date", DATE_RE)):
        if patterns.by_kind[kind]:
            for match in regex.finditer(masked):
                if digest(key, kind, match.group(0)) in patterns.by_kind[kind]:
                    hit(kind, match.start())
    if patterns.by_kind["path"]:
        for match in PATH_RE.finditer(masked):
            value = normalise("path", match.group(0))
            parts = value.split("/")
            for end in range(2, len(parts) + 1):
                if digest(key, "path", "/".join(parts[:end])) in patterns.by_kind["path"]:
                    hit("path", match.start())
    out = sorted(findings)
    if generic:
        for kind, rule in GENERIC_RULES:
            for match in rule.finditer(masked):
                out.append(f"{label}:{_line_of(masked, match.start())}: {kind} {match.group(0)!r}")
    return out


def _git(root: Path, *args: str) -> bytes:
    result = subprocess.run(["git", "-C", str(root), *args], capture_output=True)
    if result.returncode != 0:
        raise ScanError(f"git {' '.join(args)} failed: {result.stderr.decode('utf-8', 'replace').strip()}")
    return result.stdout


def tracked_files(root: Path) -> list[str]:
    return [n for n in _git(root, "ls-files", "-z").decode("utf-8").split("\0") if n]


def commit_messages(root: Path) -> list[tuple[str, str]]:
    if subprocess.run(["git", "-C", str(root), "rev-parse", "--verify", "-q", "HEAD"],
                      capture_output=True).returncode != 0:
        return []  # no commits yet
    shas = _git(root, "rev-list", "HEAD").decode().split()
    return [(sha[:12], _git(root, "log", "-1", "--format=%B", sha).decode("utf-8", "replace")) for sha in shas]


def _file_text(path: Path) -> str:
    if path.is_symlink():
        return os.readlink(path)
    data = path.read_bytes()
    if path.suffix.lower() in BINARY_SUFFIXES:
        return data.decode("latin-1")  # embedded metadata (EXIF, archive names) is ASCII
    return data.decode("utf-8", "replace")


def _load_key(args: argparse.Namespace) -> bytes:
    if args.key_file:
        raw = Path(args.key_file).read_text(encoding="utf-8").strip()
    else:
        raw = os.environ.get("DSP_PRIVATE_SCAN_KEY", "").strip()
    if len(raw) < 32:
        raise ScanError("scan key missing: set DSP_PRIVATE_SCAN_KEY or pass --key-file (CI secret)")
    return raw.encode("utf-8")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--root", type=Path, default=ROOT)
    parser.add_argument("--key-file")
    parser.add_argument("--no-commits", action="store_true")
    parser.add_argument("--hash", nargs=2, metavar=("KIND", "VALUE"))
    args = parser.parse_args(argv)
    try:
        key = _load_key(args)
        if args.hash:
            kind, value = args.hash
            if kind not in KINDS:
                raise ScanError(f"kind must be one of {', '.join(KINDS)}")
            print(f"{kind} {len(normalise(kind, value))} {digest(key, kind, value)}")
            return 0
        patterns = load_patterns()
        contexts = _owner_contexts(args.root)
        files = tracked_files(args.root)
        findings: list[str] = []
        for name in files:
            path = args.root / name
            if not (path.is_file() or path.is_symlink()):
                continue
            text = _file_text(path)
            if name == "ci/private_patterns.txt":
                # Only comments can carry prose; hash lines are validated by load_patterns().
                text = "\n".join(line if line.startswith("#") else "" for line in text.splitlines())
            findings += scan_text(name, text, key, patterns, contexts=contexts,
                                  generic=name not in GENERIC_EXEMPT)
            findings += scan_text(f"{name} (file name)", name, key, patterns, contexts=contexts)
        messages = [] if args.no_commits else commit_messages(args.root)
        for sha, body in messages:
            findings += scan_text(f"commit {sha}", body, key, patterns, contexts=contexts)
    except (ScanError, OSError) as exc:
        print(f"private_scan: {exc}", file=sys.stderr)
        return 2
    for finding in findings:
        print(finding)
    print(f"private_scan: {len(files)} files, {len(messages)} commit messages, {patterns.count} patterns, "
          f"{len(findings)} findings")
    return 1 if findings else 0


if __name__ == "__main__":
    sys.exit(main())
