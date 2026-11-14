"""Text access to the plugin's user documentation for the tests that bind its sentences to the code.

The documentation is the front page ``README.md`` plus the detail pages under ``docs/``. A sentence a test guards
may live on any of them, so ``docs_text()`` returns all pages in one string. The detail pages come first, so a
section marker such as ``**Network:**`` finds the full section and not its short form on the front page.
``doc(name)`` returns one page, for the tests that guard where a sentence stands.
"""

from __future__ import annotations

import re

from ctx_handover.tests.conftest import PLUGIN_ROOT

DOC_FILES = (
    "docs/setup.md",
    "docs/usage.md",
    "docs/reference.md",
    "docs/limits.md",
    "docs/security.md",
    "docs/development.md",
    "README.md",
)


def doc(name: str) -> str:
    """One documentation page, as written."""
    assert name in DOC_FILES, name
    return (PLUGIN_ROOT / name).read_text(encoding="utf-8")


def docs_text() -> str:
    """Every documentation page, in ``DOC_FILES`` order."""
    return "\n".join(doc(name) for name in DOC_FILES)


def flat(text: str) -> str:
    """``text`` with every run of whitespace as one space."""
    return re.sub(r"\s+", " ", text)
