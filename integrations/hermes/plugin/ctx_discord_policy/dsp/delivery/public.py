"""What a member's turn puts into Discord: no attachment directives, no host paths, no host files.

After a turn, Hermes turns parts of the reply text into uploads (``gateway/platforms/base.py``):
``MEDIA:<path>`` directives and bare local paths are attached when the file exists and is not on a short
denylist (Hermes' strict mode, off by default, narrows that to its own caches). A member's turn has no tools,
but its reply is model text: a member who gets the model to name a host path would have that file uploaded.
Two layers keep that out of public turns:

* :func:`reply` rewrites the text of a public route before Hermes extracts from it. The runner wrapper (A7)
  applies it to the turn's final answer, which every delivery path of Hermes extracts attachments from
  (the normal send, the durable queue, the rescan after a streamed answer); the delivery wrappers (K3) apply
  it to every send and edit that belongs to a public route (streamed text, interim messages). Attachment
  directives lose their colon, host paths their separators (look-alike characters that no Unicode
  normalisation turns back into ``/``, ``\\`` or ``:``). The text is matched on a per-character fold (NFKC,
  case folding, ignored format characters), so width, case and invisible-character variants of a directive
  or path are caught too. Code blocks are no exception (Hermes delivers a directive in inline code).
* :func:`make_guard` wraps the adapter's file senders (intervention A11): while a public route is in play,
  only a regular file under a root Hermes itself trusts for delivery (its media caches and the operator's
  ``media_delivery_allow_dirs``; ``gateway.platforms.base._media_delivery_allowed_roots``) may be uploaded.
  Anything else, and any failure while deciding, refuses the upload.

Operator turns are untouched: their attachments keep Hermes' own delivery rules.
"""

from __future__ import annotations

import logging
import os
import re
import unicodedata
from pathlib import Path
from typing import Any, Callable, Iterable, Optional
from urllib.parse import unquote

from .. import hermes
from ..policy.routes import Route
from ..runtime import RUNTIME
from . import worklog

logger = logging.getLogger("ctx_discord_policy")

INTERVENTION = "A11"
REFUSED_UPLOAD = "ctx_discord_policy: a non-operator turn may not upload files of the host"

SLASH, BACKSLASH, COLON = "\u2215", "\u29f5", "\u2236"   # DIVISION SLASH, REVERSE SOLIDUS OPERATOR, RATIO
_IGNORED = "\x00"            # the fold's stand-in for a format character (zero-width, bidi, soft hyphen)

# (method, the parameter that names a local file)
SENDERS: tuple[tuple[str, str], ...] = (
    ("send_document", "file_path"),
    ("send_video", "video_path"),
    ("send_voice", "audio_path"),
    ("send_image_file", "image_path"),
    ("send_multiple_images", "images"),
)


# ── text ─────────────────────────────────────────────────────────────────────────────────────────


def _fold(text: str) -> str:
    """One character per character: ASCII lower case of what NFKC and case folding make of it, so offsets
    stay those of ``text``. Format characters become ``\\x00`` (skipped by the patterns below)."""
    out = []
    for ch in text:
        if ch.isascii():
            out.append(ch.lower())
            continue
        if unicodedata.category(ch) == "Cf":
            out.append(_IGNORED)
            continue
        folded = unicodedata.normalize("NFKC", ch).casefold()
        out.append(folded[0] if folded and folded[0].isascii() else ch)
    return "".join(out)


_I = f"{_IGNORED}*"
# ``media`` + optional blanks + ``:``, followed by something a path or a quoted path can start with, or by a token
# that contains a separator or an extension (prose such as "social media: ..." stays as it is).
_DIRECTIVE = re.compile(
    rf"m{_I}e{_I}d{_I}i{_I}a{_I}[ \t]*(?P<colon>:)"
    r"(?=[ \t]*(?:[`\"'*_~/\\.]|[a-z]:|file:|[^\s`\"']*[/\\]|[^\s`\"']*\.[a-z0-9]{1,10}(?![a-z0-9])))")
# Where a host path starts (on the fold): ``file:`` URLs, UNC ``\\host``, drive ``x:\``/``x:/``, home ``~/`` or
# ``~user/``, and a ``/`` that is not inside a word, a number, a relative path or a URL's ``://``.
_ANCHOR = re.compile(
    r"(?<![a-z0-9+.\-])file:"
    r"|(?<![a-z0-9])\\\\(?=[^\s\\/])"
    r"|(?<![a-z0-9])[a-z]:[\\/]"
    r"|(?<![a-z0-9./\\])~[a-z0-9._\-]*[/\\]"
    r"|(?<![a-z0-9./\\:])/(?=[^\s/])"
    r"|(?<=:)/(?=[^\s/])(?<!://)")
_EXTENSION = re.compile(r"\.[a-z0-9]{1,10}(?![a-z0-9])")
_TOKEN_END = re.compile(r"\s")


def _token(fold: str, start: int) -> int:
    end = _TOKEN_END.search(fold, start)
    return end.start() if end is not None else len(fold)


def _looks_like_path(token: str) -> bool:
    """More than a slash command: a second separator, a drive/home/UNC/URL anchor, or an extension."""
    if not token.startswith("/"):
        return True
    body = token[1:]
    return "/" in body or "\\" in body or bool(_EXTENSION.search(body))


def neutralize_paths(text: str) -> str:
    """Directives lose their colon; host paths their separators. Idempotent."""
    if not text:
        return text
    chars = list(text)
    fold = _fold(text)
    for match in _DIRECTIVE.finditer(fold):
        chars[match.start("colon")] = COLON
    fold = _fold("".join(chars))
    for match in _ANCHOR.finditer(fold):
        start = match.start()
        end = _token(fold, start)
        token = fold[start:end]
        if not _looks_like_path(token):
            continue
        for i in range(start, end):
            if fold[i] == "/":
                chars[i] = SLASH
            elif fold[i] == "\\":
                chars[i] = BACKSLASH
            elif fold[i] == ":" and fold[max(start, i - 4):i] == "file":
                chars[i] = COLON
        fold = _fold("".join(chars))
    return "".join(chars)


def reply(text: Any, route: Any) -> Any:
    """The text a public route may deliver (see the module doc). Non-text values pass unchanged."""
    if not isinstance(text, str) or not text:
        return text
    return neutralize_paths(text)


def reply_result(result: Any, route: Any) -> Any:
    """:func:`reply` for a runner result: a plain string, or a ``str`` subclass (Hermes' ``EphemeralReply``)
    rebuilt with its time to live."""
    if not isinstance(result, str):
        return result
    clean = reply(str(result), route)
    if clean == str(result):
        return result
    if type(result) is str:
        return clean
    try:
        return type(result)(clean, getattr(result, "ttl_seconds", None))
    except Exception:
        return clean


def owner(routes: Iterable[Any]) -> Optional[Route]:
    """The route a delivery belongs to: the first of :func:`worklog.candidate_routes` (its stamp, else the turn's
    own route, else a reply anchor's routes with public ones first, so a guess never lands on the operator)."""
    return next((r for r in routes if isinstance(r, Route)), None)


def public_owner(routes: Iterable[Any]) -> Optional[Route]:
    """:func:`owner` when it is a public route, else ``None``."""
    first = owner(routes)
    return first if first is not None and not first.is_operator else None


# ── uploads ────────────────────────────────────────────────────────────────────────────────────────


def _within(path: Path, root: Path) -> bool:
    try:
        path.relative_to(root)
        return True
    except ValueError:
        return False


def trusted_file(raw: Any) -> bool:
    """A regular file under one of the roots Hermes trusts for delivery; ``False`` on any doubt."""
    try:
        text = str(raw or "")
        if text.lower().startswith("file://"):
            text = unquote(text[7:])
        if not text or "\x00" in text:
            return False
        path = Path(os.path.expanduser(text))
        if not path.is_absolute():
            return False
        resolved = path.resolve(strict=True)
        if not resolved.is_file():
            return False
        roots = hermes.media_trusted_roots()
        if not roots:
            return False
        for root in roots:
            try:
                base = Path(os.path.expanduser(str(root))).resolve(strict=False)
            except (OSError, RuntimeError, ValueError):
                continue
            if _within(resolved, base):
                return True
        return False
    except Exception:
        return False


def _local_files(name: str, value: Any) -> Optional[list[str]]:
    """Local files a sender call would upload; ``None`` when that cannot be read (closed)."""
    if name != "send_multiple_images":
        return [str(value)] if value not in (None, "") else None
    if not isinstance(value, (list, tuple)):
        return None
    out = []
    for item in value:
        url = item[0] if isinstance(item, (list, tuple)) and item else item
        text = str(url or "")
        if text.lower().startswith(("http://", "https://")):
            continue                       # downloaded by Hermes under its own URL safety check
        out.append(text)
    return out


def _in_play(adapter: Any, call: Any) -> bool:
    """A public route is in play for this delivery: the turn's own route, or a route the delivery resolves to."""
    if call is None:
        current = RUNTIME.route.get()
        return isinstance(current, Route) and not current.is_operator
    metadata = call.get("metadata")
    target = worklog.delivery_target(call.get("chat_id"), metadata)
    routes = worklog.candidate_routes(target, call.get("reply_to"), metadata, worklog.bot_of(adapter))
    return public_owner(routes) is not None


def make_guard(name: str, parameter: str) -> Callable[[Any], Any]:
    def factory(original: Any) -> Any:
        signature = worklog.signature_of(original)

        async def policy_upload(self: Any, *args: Any, **kwargs: Any) -> Any:
            call = worklog.CallArgs.bind(signature, (self, *args), kwargs)
            try:
                public = _in_play(self, call)
                files = _local_files(name, call.get(parameter)) if call is not None else None
            except Exception:
                logger.exception("ctx_discord_policy: %s upload check failed; refusing", name)
                public, files = True, None
            if public and (files is None or not all(trusted_file(f) for f in files)):
                logger.warning("ctx_discord_policy: refused %s of a non-operator turn (a file outside Hermes' "
                               "trusted media roots)", name)
                return worklog.send_result(original, success=False, error=REFUSED_UPLOAD)
            return await original(self, *args, **kwargs)

        return policy_upload

    return factory


__all__ = ["BACKSLASH", "COLON", "INTERVENTION", "REFUSED_UPLOAD", "SENDERS", "SLASH", "make_guard",
           "neutralize_paths", "owner", "public_owner", "reply", "reply_result", "trusted_file"]
