"""Real discord.py model objects (Guild, Role, Member, TextChannel, Thread) in a real client's cache, no network.

Shared by the contract tests that need discord.py's own permission semantics (S5 ingress, S-INT 2 end to end).
"""

from __future__ import annotations

from types import SimpleNamespace

GUILD, CHANNEL, MODS_CHANNEL, PRIVATE_THREAD = ("950000000000000100", "950000000000000200",
                                                "950000000000000201", "950000000000000202")
OTHER_GUILD, OTHER_CHANNEL = "950000000000000110", "950000000000000210"
CLUB_CHANNEL, CLUB_OPEN_CHANNEL, UNCACHED = "950000000000000203", "950000000000000204", "950000000000000777"
LOUNGE_CHANNEL, LOUNGE_MODS_CHANNEL, RESTRICTED_ROLE, BOTH = ("950000000000000205", "950000000000000206",
                                                             "950000000000000601", "950000000000000503")
BOT, OPERATOR, MEMBER, MOD, MODS_ROLE, EMOJI = ("950000000000000300", "950000000000000400", "950000000000000500",
                                               "950000000000000501", "950000000000000600", "950000000000000700")
ORIGINAL = 950000000000009000

READ = dict(view_channel=True, read_message_history=True, send_messages=True, send_messages_in_threads=True)


def _perm(**flags):
    import discord
    return discord.Permissions(**flags).value


def _channel_payload(cid, name, overwrites=()):
    return {"id": cid, "type": 0, "guild_id": GUILD, "name": name, "position": 0, "parent_id": None,
            "topic": None, "nsfw": False, "rate_limit_per_user": 0, "last_message_id": None, "flags": 0,
            "permission_overwrites": [{"id": entry[0], "type": entry[3] if len(entry) > 3 else 0,
                                       "allow": str(entry[1]), "deny": str(entry[2])} for entry in overwrites]}


def _member_payload(uid, roles=()):
    return {"user": {"id": uid, "username": f"user{uid[-3:]}", "global_name": None, "discriminator": "0",
                     "avatar": None, "public_flags": 0},
            "roles": list(roles), "joined_at": "2026-01-01T00:00:00+00:00", "premium_since": None, "nick": None,
            "avatar": None, "banner": None, "pending": False, "flags": 0, "deaf": False, "mute": False,
            "communication_disabled_until": None}


class DiscordWorld:
    """Real discord.py model objects in a real client's cache (no network)."""

    def __init__(self, client):
        import discord

        self.discord = discord
        state = client._connection
        state.user = discord.ClientUser(state=state, data={"id": BOT, "username": "contract-bot", "bot": True,
                                                           "global_name": None, "discriminator": "0",
                                                           "avatar": None, "public_flags": 0})
        state._users[state.user.id] = state.user
        role = lambda rid, name, perms, pos: {  # noqa: E731
            "id": rid, "name": name, "color": 0, "hoist": False, "icon": None, "unicode_emoji": None,
            "position": pos, "permissions": str(perms), "managed": False, "mentionable": False, "flags": 0,
            "colors": {"primary_color": 0, "secondary_color": None, "tertiary_color": None}}
        self.guild = discord.Guild(data={
            "id": GUILD, "name": "contract-guild", "owner_id": BOT, "emojis": [], "stickers": [], "features": [],
            "channels": [], "member_count": 4, "preferred_locale": "en-US",
            "roles": [role(GUILD, "@everyone", _perm(**READ), 0), role(MODS_ROLE, "Mods", _perm(), 1),
                      role(RESTRICTED_ROLE, "Restricted", _perm(), 2)]},
            state=state)
        state._add_guild(self.guild)
        world = self

        class Text(discord.TextChannel):
            """A real TextChannel whose message reads come from memory instead of the REST API."""

            def __init__(self, *args, **kwargs):
                super().__init__(*args, **kwargs)
                self.messages = {}
                self.fetched = []

            async def fetch_message(self, mid):
                self.fetched.append(int(mid))
                return self.messages[int(mid)]

            def history(self, **kwargs):
                items = list(self.messages.values())

                async def gen():
                    for item in reversed(items):
                        yield item

                return gen()

        class Thread(discord.Thread):
            def __init__(self, *args, **kwargs):
                super().__init__(*args, **kwargs)
                self.messages, self.fetched = {}, []

            async def fetch_message(self, mid):
                self.fetched.append(int(mid))
                return self.messages[int(mid)]

        hidden = discord.Permissions(view_channel=True).value
        self.general = Text(state=state, guild=self.guild, data=_channel_payload(CHANNEL, "general"))
        self.mods = Text(state=state, guild=self.guild, data=_channel_payload(
            MODS_CHANNEL, "mods", [(GUILD, 0, hidden), (MODS_ROLE, hidden, 0)]))
        # a mods channel that one uncached member may also read (discord.py lists them as a discord.Object)
        self.club = Text(state=state, guild=self.guild, data=_channel_payload(
            CLUB_CHANNEL, "club", [(GUILD, 0, hidden), (MODS_ROLE, hidden, 0)]))
        self.club_open = Text(state=state, guild=self.guild, data=_channel_payload(
            CLUB_OPEN_CHANNEL, "club-open", [(GUILD, 0, hidden), (MODS_ROLE, hidden, 0), (UNCACHED, hidden, 0, 1)]))
        # a lounge the Restricted role is denied; the second one also allows the Mods role explicitly
        self.lounge = Text(state=state, guild=self.guild, data=_channel_payload(
            LOUNGE_CHANNEL, "lounge", [(RESTRICTED_ROLE, 0, hidden)]))
        self.lounge_mods = Text(state=state, guild=self.guild, data=_channel_payload(
            LOUNGE_MODS_CHANNEL, "lounge-mods", [(RESTRICTED_ROLE, 0, hidden), (MODS_ROLE, hidden, 0)]))
        for channel in (self.general, self.mods, self.club, self.club_open, self.lounge, self.lounge_mods):
            self.guild._add_channel(channel)
        self.private_thread = Thread(guild=self.guild, state=state, data={
            "id": PRIVATE_THREAD, "type": 12, "guild_id": GUILD, "parent_id": CHANNEL, "owner_id": MOD,
            "name": "private", "last_message_id": None, "rate_limit_per_user": 0, "message_count": 0,
            "member_count": 1, "flags": 0, "thread_metadata": {
                "archived": False, "auto_archive_duration": 1440, "locked": False, "invitable": False,
                "archive_timestamp": "2026-01-01T00:00:00+00:00"}})
        self.guild._add_thread(self.private_thread)
        self.member = discord.Member(data=_member_payload(MEMBER), guild=self.guild, state=state)
        self.mod = discord.Member(data=_member_payload(MOD, [MODS_ROLE]), guild=self.guild, state=state)
        self.operator = discord.Member(data=_member_payload(OPERATOR, [MODS_ROLE]), guild=self.guild, state=state)
        self.both = discord.Member(data=_member_payload(BOTH, [MODS_ROLE, RESTRICTED_ROLE]), guild=self.guild,
                                   state=state)
        for m in (self.member, self.mod, self.operator, self.both):
            self.guild._add_member(m)
        world.text_class = Text
        self.other_guild = discord.Guild(data={
            "id": OTHER_GUILD, "name": "elsewhere", "owner_id": BOT, "emojis": [], "stickers": [], "features": [],
            "channels": [], "member_count": 2, "preferred_locale": "en-US",
            "roles": [role(OTHER_GUILD, "@everyone", _perm(**READ), 0)]}, state=state)
        state._add_guild(self.other_guild)
        self.elsewhere = Text(state=state, guild=self.other_guild, data={
            **_channel_payload(OTHER_CHANNEL, "elsewhere"), "guild_id": OTHER_GUILD})
        self.other_guild._add_channel(self.elsewhere)

    def post(self, channel, author, content, mid):
        message = SimpleNamespace(id=mid, channel=channel, author=author, content=content, clean_content=content,
                                  attachments=[], created_at=None, guild=self.guild, jump_url="")
        channel.messages[mid] = message
        return message
