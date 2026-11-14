"""Layer R (red) for V30 of the layer-2 contract: ``GitProbe.is_partial`` is part of the protocol
``contracts.GitProbe`` (additive, K0; wishes). E's ``gitops.GitProbe`` implements it, D's collector
asks it (W107: a partial clone travels as a file tree)."""

from __future__ import annotations

import inspect
import typing

from ctx_handover.engine import contracts, gitops
from ctx_handover.tests.fakes.planning_fake import StaticGitProbe


class _WithoutPartial:
    """A probe with the three layer-1 questions only."""

    def toplevel(self, path: str) -> str | None:
        return None

    def common_dir(self, path: str) -> str | None:
        return None

    def is_shallow(self, path: str) -> bool:
        return False


def _members(proto) -> set[str]:
    getter = getattr(typing, "get_protocol_members", None)
    if getter is not None:
        return set(getter(proto))
    return set(getattr(proto, "__protocol_attrs__", ()))


def test_git_probe_protocol_declares_is_partial():
    """``is_partial(self, path: str) -> bool`` is a member of the protocol, with E's parameter names."""
    assert "is_partial" in _members(contracts.GitProbe)
    sig = inspect.signature(contracts.GitProbe.is_partial)
    assert list(sig.parameters) == ["self", "path"]
    assert sig.return_annotation in (bool, "bool")
    assert list(inspect.signature(gitops.GitProbe.is_partial).parameters) == ["self", "path"]


def test_a_probe_without_is_partial_does_not_satisfy_the_protocol():
    """``runtime_checkable``: a probe lacking ``is_partial`` is not a ``contracts.GitProbe``; E's probe and D's
    fake are."""
    assert not isinstance(_WithoutPartial(), contracts.GitProbe)
    assert isinstance(gitops.GitProbe(gitops.Git()), contracts.GitProbe)
    assert isinstance(StaticGitProbe(), contracts.GitProbe)
