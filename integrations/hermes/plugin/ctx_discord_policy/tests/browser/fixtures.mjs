// API bodies shaped like the settings page contract (API.md); every id and name is made up.
import { NOW } from './harness.mjs'

export const BOT = '100000000000000001'
export const GUILD = '200000000000000001'
export const GUILD2 = '200000000000000002'
export const ALICE = '500000000000000001'
export const OPAL = '500000000000000002'
export const CAT_TEXT = '300000000000000100'
export const CAT_COMMUNITY = '300000000000000200'
export const ch = (n) => '3000000000000000' + String(n).padStart(2, '0')

export const iso = (ms) => new Date(ms).toISOString()
export const secs = (ms) => ms / 1000

export function meta(extra = {}) {
  return { plugin_version: '3.0.0', runtime_api: 1, api: 1, process: 'gateway', surface: 'desktop', profile: 'default',
    hermes_version: '0.23.1', mode: 'live', conflict: null,
    features: { guidance_ops: true, routing_ops: false, member_search: true, rpc: false }, ...extra }
}

export function directory(extra = {}) {
  const c = (n, name, parent, pos, more = {}) => ({ id: ch(n), name, kind: 'text', parent_id: parent,
    parent_name: parent === CAT_TEXT ? 'Text Channels' : 'Community', position: pos, can_view: true, can_send: true, ...more })
  return {
    source: 'live', at: secs(NOW), stale: false, name: 'Example Guild', icon: null,
    channels: [
      { id: CAT_TEXT, name: 'Text Channels', kind: 'category', parent_id: null, parent_name: null, position: 0, can_view: true, can_send: true },
      { id: CAT_COMMUNITY, name: 'Community', kind: 'category', parent_id: null, parent_name: null, position: 1, can_view: true, can_send: true },
      c(1, 'general', CAT_TEXT, 0), c(2, 'memes', CAT_TEXT, 1), c(3, 'help', CAT_TEXT, 2), c(4, 'off-topic', CAT_TEXT, 3),
      c(5, 'bot-spam', CAT_TEXT, 4), c(6, 'media', CAT_TEXT, 5),
      c(7, 'announcements', CAT_COMMUNITY, 0, { kind: 'announcement' }), c(8, 'events', CAT_COMMUNITY, 1),
      c(9, 'ai-worklog', CAT_COMMUNITY, 2), c(10, 'mod-log', CAT_COMMUNITY, 3, { can_send: false }),
      c(11, 'questions', CAT_COMMUNITY, 4, { kind: 'forum' }), c(12, 'rules', CAT_COMMUNITY, 5, { can_view: false })
    ],
    roles: [
      { id: GUILD, name: '@everyone', color: 0, position: 0, managed: false },
      { id: '400000000000000001', name: 'Moderators', color: 0, position: 3, managed: false },
      { id: '400000000000000002', name: 'Helpers', color: 0, position: 2, managed: false },
      { id: '400000000000000003', name: 'Lamplighter', color: 0, position: 1, managed: true }
    ],
    emojis: [{ id: '600000000000000001', name: 'lamp', animated: false, available: true }],
    ...extra
  }
}

export function schema() {
  return {
    sections: [
      { key: 'channels', label: 'Channels', note: 'Everywhere else: only when addressed (mention, reply, reaction, name).',
        fields: [
          { path: 'conversation.ambient_channels', label: 'Ambient listening', help: 'Channels the bot follows without being addressed.',
            kind: 'channel_list', max_items: 100, default: [] },
          { path: 'conversation.excluded_channels', label: 'Never respond in', help: '', kind: 'channel_list', max_items: 100, default: [] }
        ] },
      { key: 'identity', label: 'Identity', note: '', fields: [
        { path: 'identity.bot_name', label: 'Name the bot uses here', help: '', kind: 'text', max_length: 32, default: '' },
        { path: 'identity.wake_names', label: 'Names that wake it', help: '', kind: 'text_list', max_items: 5, max_length: 32, default: [] },
        { path: 'identity.greetings', label: 'Greeting words', help: '', kind: 'text_list', max_items: 12, max_length: 24, default: [] },
        { path: 'identity.reaction_emoji', label: 'Reaction emoji', help: '', kind: 'emoji', default: null },
        { path: 'conversation.language', label: 'Reply language', help: '', kind: 'text', max_length: 100, default: 'the language of the message' }
      ] },
      { key: 'guidance', label: 'Guidance', note: '', fields: [
        { path: 'prompts.guild', label: 'Server guidance', help: '', kind: 'guidance', default: '' }
      ] },
      { key: 'progress', label: 'Progress', note: '', fields: [
        { path: 'progress.grouping', label: 'Progress display', help: '', kind: 'choice', default: 'default',
          choices: [{ value: 'default', label: 'Hermes default' }, { value: 'rotate', label: 'Newest line only' },
            { value: 'separate', label: 'One per tool' }] }
      ] },
      { key: 'worklog', label: 'Worklog', note: '', fields: [
        { path: 'worklog.channel', label: 'Channel', help: '', kind: 'channel', default: null },
        { path: 'worklog.archive_minutes', label: 'Archive after', help: '', kind: 'choice', default: 1440,
          choices: [{ value: 60, label: '1 hour' }, { value: 1440, label: '24 hours' }, { value: 4320, label: '3 days' },
            { value: 10080, label: '7 days' }] },
        { path: 'worklog.final_to_origin', label: 'Final copy to the original channel', help: '', kind: 'bool', default: true }
      ] },
      { key: 'public_lanes', label: 'Public lanes', note: '', fields: [
        { path: 'lanes.channel_history.enabled', label: 'Read the current channel’s history', help: '', kind: 'bool', default: true },
        { path: 'lanes.channel_history.max_limit', label: 'History limit', help: '', kind: 'int', min: 1, max: 100, default: 100 },
        { path: 'lanes.skills', label: 'Skills members may use', help: '', kind: 'skill_list', max_items: 25, default: [] },
        { path: 'lanes.knowledge', label: 'Knowledge references', help: '', kind: 'knowledge_list', max_items: 25, default: [] }
      ] },
      { key: 'managers', label: 'Management', note: '', fields: [
        { path: 'managers.mode', label: 'Who may change settings', help: '', kind: 'choice', default: 'discord_admins',
          choices: [{ value: 'discord_admins', label: 'Server admins' }, { value: 'role', label: 'Admins with a role' },
            { value: 'operators_only', label: 'Operators only' }] },
        { path: 'managers.role_ids', label: 'Roles', help: '', kind: 'role_list', max_items: 25, default: [] },
        { path: 'audit_channel', label: 'Audit channel', help: '', kind: 'channel', default: null }
      ] },
      { key: 'templates', label: 'Templates', note: '', fields: [
        { path: 'prompts.template_overrides', label: 'Template overrides', help: '', kind: 'templates', default: {} }
      ] }
    ],
    states: ['unbound', 'active', 'paused'], archive_minutes: [60, 1440, 4320, 10080],
    groupings: ['default', 'rotate', 'separate'], manager_modes: ['discord_admins', 'role', 'operators_only'],
    placeholders: ['bot_name', 'server_name', 'channel_name', 'channel', 'language', 'worklog_channel', 'operator_label'],
    lanes: ['public', 'operator'], modes: ['ambient', 'direct', 'worklog'],
    limits: { guidance_hard: 20000, guidance_max: 6000 }
  }
}

export function guildSettings(extra = {}) {
  return {
    state: 'active', rev: 7, bound_by: 'dashboard', bound_at: iso(NOW - 86400000),
    identity: { bot_name: 'Lamp', wake_names: ['lamp'], greetings: ['hey', 'hi'], reaction_emoji: null },
    conversation: { ambient_channels: [], excluded_channels: [ch(10)], language: 'the language of the message' },
    prompts: { guild: 'Be kind.', guild_in_operator_lane: false, history: [], template_overrides: {} },
    progress: { grouping: 'default' },
    worklog: { channel: null, archive_minutes: 1440, final_to_origin: true },
    lanes: { channel_history: { enabled: true, max_limit: 100 }, permalink_hydration: true, skills: [], knowledge: [] },
    operator_lane: { channel_hydration: true },
    managers: { mode: 'discord_admins', role_ids: [] },
    audit_channel: null, updated_by: ALICE, updated_at: iso(NOW - 7200000),
    ...extra
  }
}

export function guildDetail(extra = {}) {
  const settings = guildSettings(extra.settings || {})
  return {
    bot: BOT, guild: GUILD, name: 'Example Guild', configured: true, state: settings.state, rev: settings.rev,
    settings,
    updated: { by: ALICE, label: `Discord user ${ALICE}`, via: 'discord', at: iso(NOW - 7200000) },
    bound: { by: 'dashboard', label: 'the Hermes settings page', via: 'dashboard', at: iso(NOW - 86400000) },
    findings: [],
    guidance: { chars: settings.prompts.guild.length, max_chars: 6000,
      history: [{ sha256: 'b'.repeat(64), by: 'dashboard', label: 'the Hermes settings page', at: iso(NOW - 3600000), chars: 12 }] },
    directory: { source: 'live', at: secs(NOW), stale: false },
    ...omitKey(extra, 'settings')
  }
}

function omitKey(obj, key) {
  const out = { ...obj }
  delete out[key]
  return out
}

export function overview(extra = {}) {
  return {
    profile: 'default', mode: 'live', process: 'gateway',
    store: { state: 'ok', error: null, problems: [] },
    operators: { count: 1, readable: true },
    bots: [{
      id: BOT, name: 'Lamplighter', online: true, owner_profile: 'default',
      directory: { source: 'live', at: secs(NOW), stale: false },
      guilds: [
        { id: GUILD, name: 'Example Guild', state: 'active', configured: true, rev: 7,
          updated: { by: ALICE, label: `Discord user ${ALICE}`, via: 'discord', at: iso(NOW - 7200000) },
          summary: { ambient: 2, excluded: 1, guidance_chars: 1842, guidance_max: 6000, worklog: true, managers: 'discord_admins' } },
        { id: GUILD2, name: 'Second Server', state: 'unbound', configured: false, rev: 0,
          updated: { by: null, label: 'someone', via: null, at: null },
          summary: { ambient: 0, excluded: 0, guidance_chars: 0, guidance_max: 6000, worklog: false, managers: 'discord_admins' } }
      ]
    }],
    protection: { status_at: secs(NOW - 120000), pid: 4242, hermes_version: '0.23.1', armed_profiles: ['default'],
      security_degraded: [], comfort_fallbacks: 1 },
    other_profiles: [],
    next_step: null,
    ...extra
  }
}

export function settingsBody(extra = {}) {
  return {
    values: { operators: [OPAL], frame_prompt: '', operator_label: 'the operator', guild_prompt_max_chars: 6000, notify_operators: true },
    digest: 'digest-1', readable: true, error: null,
    keys: [
      { key: 'operators', label: 'Operators', help: '', kind: 'list' },
      { key: 'frame_prompt', label: 'Operator frame', help: '', kind: 'str' },
      { key: 'operator_label', label: 'Operator label', help: '', kind: 'str' },
      { key: 'guild_prompt_max_chars', label: 'Guidance limit', help: '', kind: 'int' },
      { key: 'notify_operators', label: 'Direct-message operators', help: '', kind: 'bool' }
    ],
    ...extra
  }
}

export function previewBody(draft = '') {
  const guidance = draft ? `> ${draft}` : ''
  return {
    text: `frame\n\ncapability\n\nconversation\n\n${guidance}`,
    sections: [
      { kind: 'frame', title: 'Operator frame', text: 'You are the operator’s assistant.' },
      { kind: 'capability', title: '[Capability lane: public]', text: 'Members may read this channel.' },
      { kind: 'conversation', title: '[Conversation lane: ambient]', text: 'Join in when it helps.' },
      { kind: 'guidance', title: 'Server guidance', text: guidance }
    ],
    length: 6000 + guidance.length, budget: 9500, over_budget: false, guidance: draft ? 'included' : 'empty',
    guidance_chars: draft.length, guidance_chars_used: guidance.length, truncated_by: [],
    placeholders: { server_name: 'Example Guild' }, unknown_placeholders: /\{botname\}/.test(draft) ? ['botname'] : [], warnings: [],
    check: { chars: draft.length, max_chars: 6000, needed: guidance.length + 2, room: 3410, fits: true, problems: [],
      unknown_placeholders: /\{botname\}/.test(draft) ? ['botname'] : [] }
  }
}

// The usual route table for a configured, live server; pass overrides per test.
export function routes(over = {}) {
  return {
    'GET /v1/meta': meta(),
    'GET /v1/overview': overview(),
    'GET /v1/schema': schema(),
    [`GET /v1/guilds/${BOT}/${GUILD}`]: guildDetail(),
    [`GET /v1/directory/${BOT}/${GUILD}`]: directory(),
    'GET /v1/hermes/skills': { profile: 'default', skills: [{ name: 'faq-lookup', description: 'Answers from the FAQ', category: 'support' }], source: 'live' },
    [`GET /v1/users/${BOT}`]: { users: { [ALICE]: { name: 'alice', display_name: 'alice' }, [OPAL]: { name: 'opal', display_name: 'Opal' } } },
    'GET /v1/audit': { entries: [], guidance: [] },
    'POST /v1/preview': (req) => previewBody(req.body.draft || ''),
    'GET /v1/settings': settingsBody(),
    'GET /v1/status': { status: null, page: null, interventions: [], armed_profiles: [], at: null, pid: null, plugin_version: '3.0.0', hermes_version: '0.23.1' },
    'GET /v1/status/history': { entries: [] },
    ...over
  }
}
