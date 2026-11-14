"""Media delivery under a worklog (intervention K3, media half).

Media of an operator turn that has a worklog go to the origin channel as a reply to the triggering message
(the conversation is split, the attachment belongs next to the question). Without a worklog, media keep
stock delivery. A non-operator route never delivers media into a worklog thread.

Only ``send_voice`` honours ``reply_to``; the other senders resolve their channel through the adapter's
client, so the reply reference is added by a per-call shallow adapter copy whose client wraps exactly the
origin channel (the shared adapter is never modified).
"""

from __future__ import annotations

import copy
import logging
from typing import Any, Callable

from ..policy.routes import Route
from . import worklog

logger = logging.getLogger("ctx_discord_policy")

REFUSED = "ctx_discord_policy: a non-operator turn may not deliver into the worklog area"

# (method, positional parameters the wrapper reads, keyword parameters it reads)
MEDIA_TARGETS: tuple[tuple[str, tuple[str, ...], tuple[str, ...]], ...] = (
    ("send_multiple_images", ("chat_id", "images"), ("metadata",)),
    ("send_voice", ("chat_id", "audio_path"), ("reply_to", "metadata")),
    ("send_video", ("chat_id", "video_path"), ("reply_to", "metadata")),
    ("send_document", ("chat_id", "file_path"), ("reply_to", "metadata")),
)


class _OriginReplyChannel:
    """One origin channel whose ``send`` replies to the triggering message unless told otherwise."""

    def __init__(self, adapter: Any, channel: Any, route: Route) -> None:
        self._adapter = adapter
        self._channel = channel
        self._route = route

    def __getattr__(self, name: str) -> Any:
        return getattr(self._channel, name)

    async def send(self, *args: Any, **kwargs: Any) -> Any:
        if "reference" not in kwargs and getattr(self._adapter, "_reply_to_mode", None) != "off":
            build = getattr(self._adapter, "_message_reference_from_ids", None)
            if callable(build):
                try:
                    reference = build(self._route.message_id, self._channel)
                except Exception:
                    reference = None
                if reference is not None:
                    kwargs["reference"] = reference
        return await self._channel.send(*args, **kwargs)


class _OriginReplyClient:
    def __init__(self, adapter: Any, client: Any, route: Route) -> None:
        self._adapter = adapter
        self._client = client
        self._route = route

    def __getattr__(self, name: str) -> Any:
        return getattr(self._client, name)

    def _wrap(self, channel: Any) -> Any:
        if channel is not None and str(getattr(channel, "id", "") or "") == self._route.channel_id:
            return _OriginReplyChannel(self._adapter, channel, self._route)
        return channel

    def get_channel(self, channel_id: Any) -> Any:
        return self._wrap(self._client.get_channel(channel_id))

    async def fetch_channel(self, channel_id: Any) -> Any:
        return self._wrap(await self._client.fetch_channel(channel_id))


def origin_reply_adapter(adapter: Any, route: Route) -> Any:
    """A shallow copy of ``adapter`` whose client replies to the route's message in the origin channel."""
    client = getattr(adapter, "_client", None)
    if client is None or not callable(getattr(adapter, "_message_reference_from_ids", None)):
        return adapter
    routed = copy.copy(adapter)
    routed._client = _OriginReplyClient(adapter, client, route)
    return routed


def make_media(name: str) -> Callable[[Any], Any]:
    def factory(original: Any) -> Any:
        signature = worklog.signature_of(original)

        async def policy_media(self: Any, *args: Any, **kwargs: Any) -> Any:
            call = worklog.CallArgs.bind(signature, (self, *args), kwargs)
            if call is None:
                return await original(self, *args, **kwargs)
            metadata = call.get("metadata")
            reply_to = call.get("reply_to")
            target = worklog.delivery_target(call.get("chat_id"), metadata)
            try:
                routes = worklog.candidate_routes(target, reply_to, metadata)
            except Exception:
                logger.exception("ctx_discord_policy: %s route lookup failed; stock delivery", name)
                routes = []
            if any(not r.is_operator for r in routes):
                try:
                    hit = await worklog.public_route_hits_worklog(self, routes, target)
                except Exception:
                    logger.exception("ctx_discord_policy: %s worklog check failed; refusing", name)
                    hit = True
                if hit:
                    logger.warning("ctx_discord_policy: refused %s of a non-operator turn into worklog thread "
                                   "%s", name, target)
                    return worklog.send_result(original, success=False, error=REFUSED)
            route = worklog.route_for_target(routes, target)
            try:
                state = (await worklog.worklog_for(self, route)
                         if route is not None and worklog.is_turn_output(route, reply_to, metadata) else None)
            except Exception:
                logger.exception("ctx_discord_policy: %s worklog lookup failed; stock delivery", name)
                state = None
            if state is None or route is None:
                return await original(self, *args, **kwargs)
            # A worklog exists: the attachment goes to the origin channel, as a reply to the request.
            if call.named("reply_to") and not reply_to:
                call.set("reply_to", route.message_id)
            return await call.invoke(original, origin_reply_adapter(self, route))

        return policy_media

    return factory


__all__ = ["MEDIA_TARGETS", "REFUSED", "make_media", "origin_reply_adapter"]
