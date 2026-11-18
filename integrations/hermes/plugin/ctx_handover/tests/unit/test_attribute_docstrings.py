"""Attribute docstrings sit under the constant they describe (GottZ/ctx#354, point 2).

A string statement directly below a module-level assignment is that constant's documentation (the convention the
plugin follows and Sphinx/pydoc tooling reads). A second string after it documents nothing: in
``engine/ctxrecords.py`` the text of ``CHECKPOINT_TYPE`` stood under ``HEAD_TAG``'s own docstring. The checker
parses every runtime ``.py`` file of the plugin (everything outside ``tests/``) and reports each module-level string
statement, after the module docstring, that does not follow an assignment.
"""

from __future__ import annotations

import ast
from pathlib import Path

from ctx_handover.tests.conftest import PLUGIN_ROOT


def stray_docstrings(source: str) -> list[int]:
    """Line numbers of the module-level string statements that follow no assignment."""
    body = ast.parse(source).body
    return [
        node.lineno
        for i, node in enumerate(body)
        if i > 0
        and isinstance(node, ast.Expr)
        and isinstance(node.value, ast.Constant)
        and isinstance(node.value.value, str)
        and not isinstance(body[i - 1], (ast.Assign, ast.AnnAssign))
    ]


def test_the_checker_finds_a_docstring_under_another_docstring():
    source = '"""Module."""\nA = 1\n"""About A."""\nB = 2\n"""About B."""\n"""About A, misplaced."""\n'
    assert stray_docstrings(source) == [6]
    assert stray_docstrings('"""Module."""\nA = 1\n"""About A."""\nx: int = 2\n"""About x."""\n') == []
    assert stray_docstrings('"""Module."""\ndef f():\n    pass\n"""Below a function."""\n') == [4]


def test_every_attribute_docstring_of_the_plugin_follows_its_assignment():
    found = {
        str(path.relative_to(PLUGIN_ROOT)): lines
        for path in sorted(Path(PLUGIN_ROOT).rglob("*.py"))
        if "tests" not in path.relative_to(PLUGIN_ROOT).parts
        and (lines := stray_docstrings(path.read_text(encoding="utf-8")))
    }
    assert found == {}, f"module-level strings that document no assignment (file: lines): {found}"
