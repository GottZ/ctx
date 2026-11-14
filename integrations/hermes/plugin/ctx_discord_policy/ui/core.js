/*
 * Settings page core of the hermes-agent plugin "ctx_discord_policy" (SETTINGS-PAGE §5).
 *
 * One factory, createPolicyUI(deps), returns the page components. This file has no module statements:
 * tools/assemble_ui.py puts a head in front of it that supplies React, the host's UI kit and the plugin
 * context (desktop/plugin.js); the same core can later be wrapped for the web dashboard. Everything lives
 * inside the factory, so the file adds exactly one top-level name.
 *
 * deps = {
 *   React    the host's React (createElement and hooks)
 *   kit      {ListRow, ToggleRow, SegmentedControl, Button, Badge, Input, Textarea, EmptyState, ...};
 *            any entry may be missing, plain elements stand in for it
 *   rest     (path, {method, body, timeoutMs}) => Promise of the JSON body; failures are thrown as an
 *            Error whose message ends in "<status>: <json body>" (ctx.rest of Hermes Desktop)
 *   storage  {get(key, fallback), set(key, value), remove(key)}
 *   nav      {go(page)} with page one of overview, servers, operators, status, activity
 *   timers   {setTimeout(fn, ms), setInterval(fn, ms), addEventListener(target, type, fn)}, each
 *            returning a disposer (ctx.setTimeout, ctx.setInterval, ctx.addEventListener)
 *   now      optional () => epoch milliseconds (tests)
 * }
 *
 * Suggestions are comfort, never authority: every list field takes a typed or pasted ID, and the backend
 * validates everything again. Colour is never the only signal: every state has a word or a symbol.
 */
function createPolicyUI(deps) {
  const React = deps.React
  const h = React.createElement
  const useState = React.useState
  const useEffect = React.useEffect
  const useRef = React.useRef
  const useCallback = React.useCallback
  const kit = deps.kit || {}
  const rest = deps.rest
  const storage = deps.storage || memoryStorage()
  const nav = deps.nav || { go: function () {} }
  const timers = deps.timers || fallbackTimers()
  const now = typeof deps.now === 'function' ? deps.now : function () { return Date.now() }

  const POLL_MS = 30000
  const FOCUS_GAP_MS = 2000
  const PREVIEW_DEBOUNCE_MS = 400
  const MEMBER_DEBOUNCE_MS = 300
  const MAX_SHOWN = 50
  const UPLOAD_LIMIT = 64 * 1024
  const IMPORT_LIMIT = 1024 * 1024
  const NAME_RETRY_MS = 60000
  const PENDING = {}
  const ID_RE = /^[0-9]{17,20}$/
  const SNOWFLAKE_RE = /^[1-9][0-9]{5,19}$/
  const TOOL_RE = /^mcp__[A-Za-z0-9_-]{1,64}__(?:get|read|fetch|lookup|view|show)[A-Za-z0-9_]{0,48}$/
  const SKILL_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/
  const PLACEHOLDER_RE = /\{([^{}\s]{1,64})\}/g
  const DEFAULT_TEMPLATES = ['frame', 'ambient', 'direct', 'worklog', 'reaction_wakeup']
  const DEFAULT_PLACEHOLDERS = ['bot_name', 'server_name', 'channel_name', 'channel', 'language',
    'worklog_channel', 'operator_label']
  const PREVIEW_CHOICES = [
    { id: 'public:ambient', label: 'public · ambient' },
    { id: 'public:direct', label: 'public · direct' },
    { id: 'public:worklog', label: 'public · worklog' },
    { id: 'operator:direct', label: 'operator · direct' }
  ]
  const SECTION_TITLES = { frame: 'Operator frame', capability: 'Capability lane',
    conversation: 'Conversation lane', guidance: 'Server guidance' }
  const CODE_STATUS = { bad_request: 400, unauthorized: 401, origin: 403, not_found: 404, method: 405,
    conflict: 409, too_large: 413, json_only: 415, invalid: 422, read_only: 423, not_available: 501,
    unavailable: 503 }
  const PAGES = [
    { id: 'servers', title: 'Servers' },
    { id: 'operators', title: 'Operators' },
    { id: 'status', title: 'Status' },
    { id: 'activity', title: 'Activity' }
  ]

  // ── theme ─────────────────────────────────────────────────────────────────────────────────────────

  const V = {
    text: 'var(--ui-text-primary)',
    text2: 'var(--ui-text-secondary)',
    text3: 'var(--ui-text-tertiary)',
    stroke: 'var(--ui-stroke-secondary)',
    accent: 'var(--ui-accent)',
    fill: 'var(--ui-bg-quaternary)',
    small: 'var(--conversation-caption-font-size, 0.75rem)',
    body: 'var(--conversation-text-font-size, 0.8125rem)',
    mono: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace'
  }
  const S = {
    page: { display: 'grid', gap: '16px', paddingBottom: '48px', color: V.text, fontSize: V.body, minWidth: 0 },
    header: { display: 'flex', flexWrap: 'wrap', alignItems: 'baseline', justifyContent: 'space-between',
      gap: '6px 16px', borderBottom: '1px solid ' + V.stroke, paddingBottom: '8px' },
    h2: { margin: 0, fontSize: '1rem', fontWeight: 600 },
    h3: { margin: 0, fontSize: V.body, fontWeight: 600 },
    h4: { margin: '0 0 4px', fontSize: V.small, fontWeight: 600, color: V.text2 },
    p: { margin: 0 },
    small: { margin: 0, fontSize: V.small, color: V.text3 },
    secondary: { color: V.text2 },
    card: { border: '1px solid ' + V.stroke, borderRadius: '6px', padding: '12px 14px', display: 'grid',
      gap: '8px', minWidth: 0 },
    banner: { borderLeft: '3px solid ' + V.accent, background: V.fill, padding: '8px 12px',
      borderRadius: '4px', display: 'flex', flexWrap: 'wrap', gap: '6px', alignItems: 'baseline', margin: 0 },
    row: { display: 'flex', flexWrap: 'wrap', gap: '8px', alignItems: 'center', minWidth: 0 },
    rowEnd: { display: 'flex', flexWrap: 'wrap', gap: '8px', alignItems: 'center', justifyContent: 'flex-end' },
    list: { listStyle: 'none', margin: 0, padding: 0, display: 'grid', gap: '2px' },
    pre: { margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-word', fontFamily: V.mono, fontSize: V.small },
    input: { width: '100%', boxSizing: 'border-box', border: '1px solid ' + V.stroke, borderRadius: '4px',
      background: 'transparent', color: 'inherit', font: 'inherit', padding: '4px 8px' },
    textarea: { width: '100%', boxSizing: 'border-box', fontFamily: V.mono, fontSize: V.small, lineHeight: 1.5,
      minHeight: '14em', resize: 'vertical', whiteSpace: 'pre-wrap' },
    table: { width: '100%', borderCollapse: 'collapse', fontSize: V.small },
    th: { textAlign: 'left', fontWeight: 600, padding: '4px 8px', borderBottom: '1px solid ' + V.stroke,
      color: V.text2 },
    td: { padding: '4px 8px', borderBottom: '1px solid ' + V.stroke, verticalAlign: 'top' },
    error: { margin: 0, fontSize: V.small, fontWeight: 600 },
    warning: { margin: 0, fontSize: V.small },
    chip: { display: 'inline-flex', alignItems: 'center', gap: '2px', padding: '1px 6px', borderRadius: '3px',
      background: V.fill, color: V.text, border: '1px solid ' + V.stroke, font: 'inherit', fontSize: V.small,
      cursor: 'pointer', maxWidth: '100%' },
    comboInput: { flex: '1 1 8em', minWidth: '6em', border: 'none', outline: 'none', background: 'transparent',
      color: 'inherit', font: 'inherit', padding: '2px 0' },
    listbox: { border: '1px solid ' + V.stroke, borderRadius: '4px', maxHeight: '240px', overflowY: 'auto',
      padding: '2px 0' },
    groupLabel: { padding: '6px 8px 2px', fontSize: V.small, color: V.text3, fontWeight: 600 },
    linkButton: { background: 'transparent', border: 'none', padding: 0, color: 'inherit', font: 'inherit',
      textDecoration: 'underline', cursor: 'pointer', textAlign: 'left' },
    fallbackButton: { display: 'inline-flex', alignItems: 'center', gap: '4px', padding: '3px 10px',
      borderRadius: '3px', border: '1px solid ' + V.stroke, background: V.fill, color: V.text, font: 'inherit',
      fontSize: V.small, cursor: 'pointer' },
    details: { border: '1px solid ' + V.stroke, borderRadius: '6px', padding: '8px 14px' },
    summary: { cursor: 'pointer', fontWeight: 600 }
  }

  function boxStyle(focused, invalid, disabled) {
    return { display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: '4px', padding: '3px 6px',
      minHeight: '30px', boxSizing: 'border-box', border: '1px ' + (invalid ? 'dashed ' : 'solid ') +
        (focused ? V.accent : V.stroke), borderRadius: '4px', boxShadow: focused ? '0 0 0 1px ' + V.accent : 'none',
      opacity: disabled ? 0.6 : 1, cursor: disabled ? 'default' : 'text', minWidth: 0 }
  }

  function optionStyle(active) {
    return { padding: '4px 8px', cursor: 'pointer', display: 'flex', gap: '6px', alignItems: 'baseline',
      flexWrap: 'wrap', background: active ? V.fill : 'transparent',
      borderLeft: '3px solid ' + (active ? V.accent : 'transparent') }
  }

  // ── small helpers ─────────────────────────────────────────────────────────────────────────────────

  function memoryStorage() {
    const data = {}
    return {
      get: function (key, fallback) { return Object.prototype.hasOwnProperty.call(data, key) ? data[key] : fallback },
      set: function (key, value) { data[key] = value },
      remove: function (key) { delete data[key] }
    }
  }

  // Without the host's tracked timers only one-shot timers work (debounce); nothing polls.
  function fallbackTimers() {
    return {
      setTimeout: function (fn, ms) {
        const id = globalThis.setTimeout(fn, ms)
        return function () { globalThis.clearTimeout(id) }
      },
      setInterval: function () { return function () {} },
      addEventListener: function () { return function () {} }
    }
  }

  function disposer(value) { return typeof value === 'function' ? value : function () {} }
  function later(fn, ms) { return disposer(timers.setTimeout(fn, ms)) }

  function onWindowFocus(fn) {
    if (typeof window === 'undefined' || typeof timers.addEventListener !== 'function') return function () {}
    return disposer(timers.addEventListener(window, 'focus', fn))
  }

  function sget(key, fallback) {
    try {
      const value = storage.get(key, fallback)
      return value === undefined ? fallback : value
    } catch (e) {
      return fallback
    }
  }
  function sset(key, value) { try { storage.set(key, value) } catch (e) { /* comfort only */ } }
  function sremove(key) { try { storage.remove(key) } catch (e) { /* comfort only */ } }

  function hasOwn(obj, key) { return obj != null && Object.prototype.hasOwnProperty.call(obj, key) }

  function getPath(obj, path) {
    let cur = obj
    const parts = String(path).split('.')
    for (let i = 0; i < parts.length; i++) {
      if (cur == null || typeof cur !== 'object') return undefined
      cur = cur[parts[i]]
    }
    return cur
  }

  function equal(a, b) {
    if (a === b) return true
    if (a == null || b == null || typeof a !== 'object' || typeof b !== 'object') return false
    if (Array.isArray(a) !== Array.isArray(b)) return false
    const ka = Object.keys(a)
    const kb = Object.keys(b)
    if (ka.length !== kb.length) return false
    for (let i = 0; i < ka.length; i++) {
      if (!hasOwn(b, ka[i]) || !equal(a[ka[i]], b[ka[i]])) return false
    }
    return true
  }

  function clone(value) { return value == null ? value : JSON.parse(JSON.stringify(value)) }
  function omit(obj, keys) {
    const out = Object.assign({}, obj)
    keys.forEach(function (k) { delete out[k] })
    return out
  }
  function enc(value) { return encodeURIComponent(String(value)) }
  function gpath(bot, guild) { return '/v1/guilds/' + enc(bot) + '/' + enc(guild) }
  function domId(prefix, value) { return prefix + '-' + String(value).replace(/[^A-Za-z0-9_-]/g, '-') }
  function joinIds() {
    const ids = Array.prototype.slice.call(arguments).filter(Boolean)
    return ids.length ? ids.join(' ') : undefined
  }

  let uidCounter = 0
  function useUid(prefix) {
    const ref = useRef(null)
    if (ref.current === null) {
      uidCounter += 1
      ref.current = prefix + '-' + uidCounter
    }
    return ref.current
  }

  function fmtN(n) {
    const num = Number(n)
    return Number.isFinite(num) ? num.toLocaleString('en-US') : '?'
  }
  function plural(n, one, many) { return fmtN(n) + ' ' + (Number(n) === 1 ? one : many) }

  function toMs(at) {
    if (at == null || at === '') return null
    if (typeof at === 'number') return Number.isFinite(at) ? (at > 1e12 ? at : at * 1000) : null
    const text = String(at).trim()
    if (/^[0-9]+(\.[0-9]+)?$/.test(text)) {
      const num = Number(text)
      return num > 1e12 ? num : num * 1000
    }
    const parsed = Date.parse(text)
    return Number.isFinite(parsed) ? parsed : null
  }
  function two(n) { return n < 10 ? '0' + n : String(n) }
  function fmtUTC(at) {
    const ms = toMs(at)
    if (ms == null) return 'an unknown time'
    const d = new Date(ms)
    return d.getUTCFullYear() + '-' + two(d.getUTCMonth() + 1) + '-' + two(d.getUTCDate()) + ' ' +
      two(d.getUTCHours()) + ':' + two(d.getUTCMinutes()) + ' UTC'
  }
  // Short form for badges: the clock time on the current UTC day, the full date otherwise.
  function fmtShort(at) {
    const ms = toMs(at)
    if (ms == null) return 'an unknown time'
    const d = new Date(ms)
    const today = new Date(now())
    if (d.getUTCFullYear() === today.getUTCFullYear() && d.getUTCMonth() === today.getUTCMonth() &&
      d.getUTCDate() === today.getUTCDate()) {
      return two(d.getUTCHours()) + ':' + two(d.getUTCMinutes()) + ' UTC'
    }
    return fmtUTC(at)
  }
  function relTime(at) {
    const ms = toMs(at)
    if (ms == null) return 'at an unknown time'
    const s = Math.round((now() - ms) / 1000)
    if (s < 5) return 'just now'
    if (s < 60) return plural(s, 'second', 'seconds') + ' ago'
    if (s < 3600) return plural(Math.floor(s / 60), 'minute', 'minutes') + ' ago'
    if (s < 86400) return plural(Math.floor(s / 3600), 'hour', 'hours') + ' ago'
    if (s < 30 * 86400) return plural(Math.floor(s / 86400), 'day', 'days') + ' ago'
    return 'on ' + fmtUTC(at).slice(0, 10)
  }

  function viaText(via) {
    if (!via) return ''
    return { discord: 'Discord', dashboard: 'settings page', cli: 'host command line' }[via] || String(via)
  }

  function whoText(who, nameOf) {
    if (!who) return 'someone'
    const by = who.by == null ? '' : String(who.by)
    if (SNOWFLAKE_RE.test(by) && nameOf) {
      const name = nameOf(by)
      if (name) return '@' + name
    }
    if (who.label) return String(who.label)
    if (by === 'dashboard') return 'the Hermes settings page'
    if (by === 'cli') return 'the host command line'
    if (SNOWFLAKE_RE.test(by)) return 'Discord user ' + by
    return 'someone'
  }

  function shortId(id) {
    const text = String(id)
    return text.length > 6 ? text.slice(0, 3) + '…' : text
  }

  function download(name, text, type) {
    if (typeof document === 'undefined' || typeof URL === 'undefined' || typeof URL.createObjectURL !== 'function') {
      return false
    }
    const url = URL.createObjectURL(new Blob([text], { type: type || 'text/plain' }))
    const a = document.createElement('a')
    a.href = url
    a.download = name
    a.rel = 'noopener'
    a.style.display = 'none'
    document.body.appendChild(a)
    a.click()
    a.remove()
    later(function () { URL.revokeObjectURL(url) }, 1000)
    return true
  }

  function readTextFile(file) {
    if (!file) return Promise.resolve({ error: 'No file was chosen.' })
    if (file.size > UPLOAD_LIMIT) {
      return Promise.resolve({ error: 'The file is larger than 64 KB; nothing was loaded.' })
    }
    const buffer = typeof file.arrayBuffer === 'function' ? file.arrayBuffer() : new Promise(function (resolve, reject) {
      const reader = new FileReader()
      reader.onload = function () { resolve(reader.result) }
      reader.onerror = function () { reject(reader.error) }
      reader.readAsArrayBuffer(file)
    })
    return buffer.then(function (buf) {
      try {
        const text = new TextDecoder('utf-8', { fatal: true }).decode(buf)
        return { text: text.replace(/^﻿/, '').replace(/\r\n?/g, '\n') }
      } catch (e) {
        return { error: 'The file is not UTF-8 text; nothing was loaded.' }
      }
    }, function () { return { error: 'The file could not be read.' } })
  }

  // A settings file for import: read and parsed in the browser (≤ 1 MB, the backend's import limit); the
  // backend validates it like every other write.
  function readJsonFile(file) {
    if (!file) return Promise.resolve({ error: 'no file was chosen.' })
    if (file.size > IMPORT_LIMIT) return Promise.resolve({ error: 'the file is larger than 1 MB.' })
    const content = typeof file.text === 'function' ? file.text() : new Promise(function (resolve, reject) {
      const reader = new FileReader()
      reader.onload = function () { resolve(reader.result) }
      reader.onerror = function () { reject(reader.error) }
      reader.readAsText(file, 'utf-8')
    })
    return content.then(function (raw) {
      try {
        return { data: JSON.parse(String(raw).replace(/^﻿/, '')) }
      } catch (e) {
        return { error: 'the file is not JSON.' }
      }
    }, function () { return { error: 'the file could not be read.' } })
  }

  // ── talking to the backend ────────────────────────────────────────────────────────────────────────

  function errorResult(status, body) {
    const err = body && typeof body.error === 'object' && body.error ? body.error : {}
    const message = err.message || (body && typeof body.error === 'string' ? body.error : '') || ('HTTP ' + status)
    const pick = function (key) { return body && body[key] !== undefined ? body[key] : err[key] }
    return {
      ok: false, transport: false, status: status, body: body || {}, code: err.code || '', message: message,
      field: err.field || null, findings: Array.isArray(err.findings) ? err.findings : [],
      conflict: status === 409 ? { rev: pick('current_rev'), by: pick('updated_by'), label: pick('updated_by_label'),
        at: pick('updated_at'), via: pick('updated_via'), digest: pick('digest') } : null
    }
  }

  // ctx.rest throws Error("404: <body>") or Error("... Error: 409: <body>"). The status is the last
  // "<3 digits>: " whose remainder parses as a JSON object; anything else is a transport error.
  function parseRestError(error) {
    const text = error && typeof error.message === 'string' ? error.message : String(error)
    const re = /(?<![0-9])([0-9]{3}): /g
    const hits = []
    let m = re.exec(text)
    while (m !== null) {
      hits.push(m)
      m = re.exec(text)
    }
    for (let i = hits.length - 1; i >= 0; i--) {
      const status = Number(hits[i][1])
      if (status < 100 || status > 599) continue
      const tail = text.slice(hits[i].index + hits[i][0].length).trim()
      if (tail.charAt(0) !== '{') continue
      try {
        return errorResult(status, JSON.parse(tail))
      } catch (e) { /* not the body: keep looking further left */ }
    }
    return { ok: false, transport: true, status: hits.length ? Number(hits[hits.length - 1][1]) : 0, body: null,
      code: 'transport', message: text, field: null, findings: [], conflict: null }
  }

  function call(path, opts) {
    let pending
    try {
      pending = Promise.resolve(rest(path, opts))
    } catch (e) {
      pending = Promise.reject(e)
    }
    return pending.then(function (data) {
      if (data && typeof data === 'object' && data.ok === false && data.error) {
        const code = typeof data.error === 'object' ? data.error.code : data.error
        return errorResult(Number(data.status) || CODE_STATUS[code] || 400, data)
      }
      return { ok: true, status: 200, data: data }
    }, parseRestError)
  }

  function errorText(r) {
    if (!r) return ''
    if (r.transport) {
      return 'The plugin backend did not answer' + (r.status ? ' (HTTP ' + r.status + ')' : '') + ': ' + r.message
    }
    return r.message || 'HTTP ' + r.status
  }

  const cache = new Map()

  function useLoad(path, opts) {
    const poll = !(opts && opts.poll === false)
    const [state, setState] = useState(function () {
      return { path: path, loading: !!path, data: path && cache.has(path) ? cache.get(path) : null, error: null }
    })
    const seq = useRef(0)
    const alive = useRef(true)
    const last = useRef(0)
    const load = useCallback(function () {
      if (!path) return Promise.resolve(null)
      seq.current += 1
      const n = seq.current
      last.current = now()
      return call(path).then(function (r) {
        if (!alive.current || n !== seq.current) return r
        if (r.ok) {
          cache.set(path, r.data)
          setState({ path: path, loading: false, data: r.data, error: null })
        } else {
          setState(function (prev) {
            return { path: path, loading: false, data: prev.path === path ? prev.data : null, error: r }
          })
        }
        return r
      })
    }, [path])
    useEffect(function () {
      alive.current = true
      if (!path) return function () { alive.current = false }
      setState(function (prev) {
        return prev.path === path ? prev : { path: path, loading: true, data: cache.has(path) ? cache.get(path) : null,
          error: null }
      })
      load()
      if (!poll) return function () { alive.current = false }
      const stopPoll = disposer(timers.setInterval(load, POLL_MS))
      const stopFocus = onWindowFocus(function () { if (now() - last.current >= FOCUS_GAP_MS) load() })
      return function () {
        alive.current = false
        stopPoll()
        stopFocus()
      }
    }, [path, poll, load])
    const same = state.path === path
    return { data: same ? state.data : null, error: same ? state.error : null, loading: same ? state.loading : !!path,
      reload: load }
  }

  // bot:id -> display name, or PENDING while it is asked for or could not be given (asked again a minute later).
  const userNames = new Map()

  // Display names for Discord user ids (comfort: the id stays visible when the name is unknown).
  function useUserNames(bot, ids) {
    const [, redraw] = useState(0)
    const [round, nextRound] = useState(0)        // bumped only by the retry timer: a new request round
    const key = (ids || []).filter(function (id) { return SNOWFLAKE_RE.test(String(id)) }).map(String).sort().join(',')
    useEffect(function () {
      if (!bot || !key) return undefined
      const missing = key.split(',').filter(function (id) { return !userNames.has(bot + ':' + id) })
      if (!missing.length) return undefined
      missing.forEach(function (id) { userNames.set(bot + ':' + id, PENDING) })
      let alive = true
      let stopRetry = null
      const forget = function () {          // unresolved names may be asked for again by the next request
        missing.forEach(function (id) { if (userNames.get(bot + ':' + id) === PENDING) userNames.delete(bot + ':' + id) })
      }
      call('/v1/users/' + enc(bot) + '?ids=' + missing.join(',')).then(function (r) {
        const users = r.ok && r.data && r.data.users ? r.data.users : {}
        Object.keys(users).forEach(function (id) {
          const u = users[id] || {}
          if (u.display_name || u.name) userNames.set(bot + ':' + id, u.display_name || u.name)
        })
        if (!alive) {
          forget()
          return
        }
        redraw(function (x) { return x + 1 })
        if (missing.some(function (id) { return userNames.get(bot + ':' + id) === PENDING })) {
          stopRetry = later(function () {
            stopRetry = null
            forget()
            if (alive) nextRound(function (x) { return x + 1 })
          }, NAME_RETRY_MS)
        }
      })
      return function () {
        alive = false
        if (stopRetry) {
          stopRetry()
          forget()
        }
      }
    }, [bot, key, round])
    return function (id) {
      const v = bot ? userNames.get(bot + ':' + id) : null
      return typeof v === 'string' ? v : null
    }
  }

  // The server picked on the Servers page; the overview sets it before navigating there.
  const selectionListeners = new Set()
  function readSelection() {
    const sel = sget('server', null)
    return sel && typeof sel === 'object' && sel.bot && sel.guild ? { bot: String(sel.bot), guild: String(sel.guild) } : null
  }
  function writeSelection(sel) {
    if (sel) sset('server', { bot: sel.bot, guild: sel.guild })
    else sremove('server')
    selectionListeners.forEach(function (fn) { fn(sel) })
  }
  function openServer(bot, guild) {
    writeSelection({ bot: String(bot), guild: String(guild) })
    nav.go('servers')
  }

  let resetUnavailable = false

  // ── kit with plain fallbacks ──────────────────────────────────────────────────────────────────────

  function FallbackButton(props) {
    const style = Object.assign({}, S.fallbackButton, props.variant === 'default' ? { fontWeight: 600,
      borderColor: V.accent } : null, props.disabled ? { opacity: 0.5, cursor: 'default' } : null, props.style)
    const rest = omit(props, ['variant', 'size', 'loading', 'style', 'children'])
    return h('button', Object.assign({ type: 'button' }, rest, { style: style }), props.children)
  }

  function FallbackListRow(props) {
    return h('div', { id: props.id, style: { display: 'grid', gap: '6px', padding: '10px 0', minWidth: 0 } },
      h('div', { style: { display: 'flex', flexWrap: 'wrap', gap: '8px 16px', alignItems: 'center',
        justifyContent: 'space-between' } },
        h('div', { style: { minWidth: 0, flex: '1 1 16em' } },
          h('div', { style: { fontWeight: 500 } }, props.title),
          props.description ? h('div', { style: S.small }, props.description) : null),
        props.action ? h('div', { style: S.rowEnd }, props.action) : null),
      props.below || null)
  }

  function FallbackToggleRow(props) {
    return h(FallbackListRow, {
      id: props.id, title: props.label, description: props.description, below: props.below,
      action: h('button', { type: 'button', role: 'switch', 'aria-checked': props.checked ? 'true' : 'false',
        'aria-label': props.label, disabled: props.disabled, style: S.fallbackButton,
        onClick: function () { props.onChange(!props.checked) } }, props.checked ? 'On' : 'Off')
    })
  }

  function FallbackSegmented(props) {
    return h('div', { style: { display: 'inline-flex', flexWrap: 'wrap', gap: '2px' } },
      props.options.map(function (o) {
        const on = props.value === o.id
        return h('button', { key: o.id, type: 'button', 'aria-pressed': on ? 'true' : 'false', disabled: props.disabled,
          onClick: function () { props.onChange(o.id) },
          style: Object.assign({}, S.fallbackButton, on ? { fontWeight: 600, borderColor: V.accent } : null) },
        (on ? '● ' : '') + o.label)
      }))
  }

  function FallbackBadge(props) {
    return h('span', { style: { display: 'inline-flex', alignItems: 'center', gap: '4px', padding: '1px 6px',
      borderRadius: '3px', border: '1px solid ' + V.stroke, fontSize: V.small, fontWeight: 500 } }, props.children)
  }

  function FallbackEmpty(props) {
    return h('div', { style: { padding: '24px 8px', textAlign: 'center' } },
      h('div', { style: { fontWeight: 600 } }, props.title),
      props.description ? h('div', { style: S.small }, props.description) : null)
  }

  const K = {
    Button: kit.Button || FallbackButton,
    ListRow: kit.ListRow || FallbackListRow,
    ToggleRow: kit.ToggleRow || FallbackToggleRow,
    Segmented: kit.SegmentedControl || FallbackSegmented,
    Badge: kit.Badge || FallbackBadge,
    EmptyState: kit.EmptyState || FallbackEmpty,
    Input: kit.Input || 'input',
    Textarea: kit.Textarea || 'textarea'
  }

  function Btn(props) {
    return h(K.Button, Object.assign({ type: 'button', variant: 'secondary', size: 'sm' }, props))
  }

  function TextInput(props) {
    const style = Object.assign({}, kit.Input ? null : S.input, props.style)
    return h(K.Input, Object.assign({ type: 'text', autoComplete: 'off', spellCheck: false }, props, { style: style }))
  }

  function TextArea(props) {
    const style = Object.assign({}, kit.Textarea ? null : S.input, S.textarea, props.style)
    return h(K.Textarea, Object.assign({ spellCheck: false }, props, { style: style }))
  }

  function Banner(props) {
    return h('div', { role: props.role || 'status', style: Object.assign({}, S.banner, props.style) }, props.children)
  }

  function Segmented(props) {
    return h('div', { id: props.id, role: 'group', 'aria-labelledby': props.labelledBy, 'aria-label': props.label,
      'aria-describedby': props.describedBy },
      h(K.Segmented, { options: props.options, value: props.value, onChange: props.onChange, disabled: props.disabled }))
  }

  // ── combobox (WAI-ARIA editable combobox with list autocomplete) ──────────────────────────────────

  /*
   * props: id (of the input), labelledBy, selected [{key, label}], options [{key, label, hint?, group?,
   * terms?}], onAdd(option), onRemove(key), single, limit, noun [singular, plural], placeholder,
   * freeOptions(query, chosenKeys) => [option], filter (false = options are already search results),
   * onQueryChange(query), liveText (overrides the count), describedBy, invalid, disabled.
   */
  function Combobox(props) {
    const id = props.id
    const selected = props.selected || []
    const options = props.options || []
    const noun = props.noun || ['option', 'options']
    const [query, setQuery] = useState('')
    const [open, setOpen] = useState(false)
    const [active, setActive] = useState(-1)
    const [focused, setFocused] = useState(false)
    const inputRef = useRef(null)
    const listRef = useRef(null)
    const listId = id + '-listbox'
    const liveId = id + '-live'
    const chosen = new Set(selected.map(function (s) { return String(s.key) }))
    const full = !props.single && props.limit != null && selected.length >= props.limit
    const q = query.trim().toLowerCase()
    const matches = options.filter(function (o) {
      if (chosen.has(String(o.key))) return false
      if (props.filter === false || !q) return true
      return String(o.label).toLowerCase().indexOf(q) >= 0 || String(o.key).toLowerCase().indexOf(q) >= 0 ||
        (o.terms ? String(o.terms).toLowerCase().indexOf(q) >= 0 : false)
    })
    const free = props.freeOptions && query.trim() ? props.freeOptions(query.trim(), chosen) || [] : []
    const shown = full ? [] : free.concat(matches.slice(0, MAX_SHOWN))
    const expanded = open && shown.length > 0
    const act = active >= 0 && active < shown.length ? active : -1
    const optId = function (i) { return id + '-option-' + i }

    useEffect(function () {
      if (!expanded || act < 0 || !listRef.current) return
      const el = listRef.current.querySelector('#' + optId(act))
      if (el && typeof el.scrollIntoView === 'function') el.scrollIntoView({ block: 'nearest' })
    }, [act, expanded])

    let live = ''
    if (open) {
      if (props.liveText) live = props.liveText
      else if (full) live = 'Limit of ' + props.limit + ' reached; remove one to add another.'
      else if (matches.length === 0) live = 'No ' + noun[1] + ' found'
      else {
        live = plural(matches.length, noun[0], noun[1]) + ' found'
        if (matches.length > MAX_SHOWN) live += ', showing the first ' + MAX_SHOWN + '; type to narrow'
      }
    }

    function setQ(value) {
      setQuery(value)
      if (props.onQueryChange) props.onQueryChange(value)
    }

    function accept(option) {
      props.onAdd(option)
      setQ('')
      setOpen(false)
      setActive(-1)
    }

    function onKeyDown(e) {
      const n = shown.length
      switch (e.key) {
        case 'ArrowDown':
          e.preventDefault()
          if (!open || act < 0) {
            setOpen(true)
            setActive(n ? 0 : -1)
          } else {
            setActive(n ? (act + 1) % n : -1)
          }
          break
        case 'ArrowUp':
          e.preventDefault()
          if (!open || act < 0) {
            setOpen(true)
            setActive(n ? n - 1 : -1)
          } else {
            setActive(n ? (act <= 0 ? n - 1 : act - 1) : -1)
          }
          break
        case 'Home':
          if (expanded) {
            e.preventDefault()
            setActive(0)
          }
          break
        case 'End':
          if (expanded) {
            e.preventDefault()
            setActive(n - 1)
          }
          break
        case 'Enter':
          if (expanded && act >= 0) {
            e.preventDefault()
            accept(shown[act])
          } else if (free.length && !full) {
            e.preventDefault()
            accept(free[0])
          }
          break
        case 'Escape':
          if (open) {
            e.preventDefault()
            e.stopPropagation()
            setOpen(false)
            setActive(-1)
          } else if (query) {
            e.preventDefault()
            e.stopPropagation()
            setQ('')
          }
          break
        case 'Tab':
          setOpen(false)
          setActive(-1)
          break
        case 'Backspace':
          if (!query && selected.length && !props.disabled) {
            e.preventDefault()
            props.onRemove(selected[selected.length - 1].key)
          }
          break
        default:
          break
      }
    }

    function optionEl(o, i) {
      return h('div', {
        key: 'o' + i + '-' + o.key, id: optId(i), role: 'option', 'aria-selected': i === act ? 'true' : 'false',
        style: optionStyle(i === act),
        onMouseDown: function (e) { e.preventDefault() },
        onMouseMove: function () { if (i !== act) setActive(i) },
        onClick: function () { accept(o) }
      },
      h('span', null, o.label),
      o.hint ? h('span', { style: S.small }, ' · ' + o.hint) : null)
    }

    function renderOptions() {
      const out = []
      let i = 0
      while (i < shown.length) {
        const group = shown[i].group
        if (!group) {
          out.push(optionEl(shown[i], i))
          i += 1
          continue
        }
        const start = i
        const items = []
        while (i < shown.length && shown[i].group === group) {
          items.push(optionEl(shown[i], i))
          i += 1
        }
        const gid = id + '-group-' + start
        out.push(h('div', { key: gid, role: 'group', 'aria-labelledby': gid },
          h('div', { id: gid, role: 'presentation', style: S.groupLabel }, group), items))
      }
      return out
    }

    return h('div', { style: { display: 'grid', gap: '4px', minWidth: 0 } },
      h('div', {
        style: boxStyle(focused, props.invalid, props.disabled),
        onMouseDown: function (e) {
          if (e.target === e.currentTarget && inputRef.current) {
            e.preventDefault()
            inputRef.current.focus()
          }
        }
      },
      selected.map(function (s) {
        return h('button', {
          key: 'chip-' + s.key, type: 'button', 'aria-label': 'Remove ' + s.label, title: s.title,
          disabled: props.disabled, style: S.chip,
          onClick: function () {
            props.onRemove(s.key)
            if (inputRef.current) inputRef.current.focus()
          }
        }, h('span', null, s.label), h('span', { 'aria-hidden': 'true' }, ' ×'))
      }),
      h('input', {
        ref: inputRef, id: id, type: 'text', role: 'combobox', autoComplete: 'off', spellCheck: false,
        'aria-expanded': expanded ? 'true' : 'false', 'aria-controls': listId, 'aria-autocomplete': 'list',
        'aria-activedescendant': expanded && act >= 0 ? optId(act) : undefined,
        'aria-labelledby': props.labelledBy, 'aria-describedby': joinIds(props.describedBy, liveId),
        'aria-invalid': props.invalid ? 'true' : undefined, disabled: props.disabled, value: query,
        placeholder: props.placeholder || 'Search…', style: S.comboInput,
        onChange: function (e) {
          setQ(e.target.value)
          setOpen(true)
          setActive(-1)
        },
        onKeyDown: onKeyDown,
        onFocus: function () { setFocused(true) },
        onBlur: function () {
          setFocused(false)
          setOpen(false)
          setActive(-1)
        },
        onClick: function () { if (!props.disabled) setOpen(true) }
      }),
      h('button', {
        type: 'button', tabIndex: -1, 'aria-label': (expanded ? 'Hide ' : 'Show ') + noun[1],
        disabled: props.disabled, style: Object.assign({}, S.linkButton, { textDecoration: 'none', padding: '0 4px' }),
        onMouseDown: function (e) { e.preventDefault() },
        onClick: function () {
          setOpen(!open)
          setActive(-1)
          if (inputRef.current) inputRef.current.focus()
        }
      }, h('span', { 'aria-hidden': 'true' }, '▾'))),
      h('div', { ref: listRef, id: listId, role: 'listbox', 'aria-labelledby': props.labelledBy, hidden: !expanded,
        style: S.listbox }, expanded ? renderOptions() : null),
      h('div', { id: liveId, role: 'status', 'aria-live': 'polite', style: S.small }, live))
  }

  function idOption(text, chosen, known) {
    if (!ID_RE.test(text) || chosen.has(text) || (known && known.has(text))) return []
    return [{ key: text, label: 'Use ID ' + text, free: true }]
  }

  // ── suggestion sources ────────────────────────────────────────────────────────────────────────────

  function directorySource(dir) {
    if (!dir) return 'IDs only'
    if (dir.source === 'live' || dir.source === 'rpc') return 'live'
    if (dir.source === 'snapshot') return 'directory of ' + fmtShort(dir.at) + (dir.stale ? ' (old)' : '')
    return 'IDs only'
  }

  function channelOptions(dir) {
    const channels = (dir && Array.isArray(dir.channels)) ? dir.channels : []
    const categories = new Map()
    channels.forEach(function (c) { if (c.kind === 'category') categories.set(String(c.id), c) })
    const grouped = channels.some(function (c) { return c.kind !== 'category' && c.parent_id })
    const catPos = function (c) {
      const parent = c.parent_id ? categories.get(String(c.parent_id)) : null
      return parent ? (Number(parent.position) || 0) + 1 : (c.parent_id ? 1e6 : 0)
    }
    return channels.filter(function (c) { return c.kind !== 'category' }).slice().sort(function (a, b) {
      return catPos(a) - catPos(b) || (Number(a.position) || 0) - (Number(b.position) || 0) ||
        String(a.name).localeCompare(String(b.name))
    }).map(function (c) {
      const parent = c.parent_id ? categories.get(String(c.parent_id)) : null
      const hints = []
      if (c.kind && c.kind !== 'text') hints.push(String(c.kind))
      if (c.can_view === false) hints.push('the bot cannot see it')
      else if (c.can_send === false) hints.push('the bot cannot write here')
      return { key: String(c.id), label: '#' + (c.name || c.id), hint: hints.join(' · '),
        group: grouped ? (c.parent_id ? (c.parent_name || (parent && parent.name) || 'Other') : 'No category') : undefined }
    })
  }

  function roleOptions(dir, guild) {
    const roles = (dir && Array.isArray(dir.roles)) ? dir.roles : []
    return roles.filter(function (r) { return String(r.id) !== String(guild) && r.name !== '@everyone' })
      .slice().sort(function (a, b) { return (Number(b.position) || 0) - (Number(a.position) || 0) })
      .map(function (r) {
        return { key: String(r.id), label: '@' + (r.name || r.id), hint: r.managed ? 'managed by an integration' : '' }
      })
  }

  function emojiOptions(dir) {
    const emojis = (dir && Array.isArray(dir.emojis)) ? dir.emojis : []
    return emojis.map(function (e) {
      const hints = []
      if (e.animated) hints.push('animated')
      if (e.available === false) hints.push('not available')
      return { key: String(e.id), label: ':' + (e.name || e.id) + ':', value: { id: String(e.id), name: e.name },
        hint: hints.join(' · ') }
    })
  }

  function skillOptions(skills) {
    const list = (skills && Array.isArray(skills.skills)) ? skills.skills : []
    return list.map(function (s) {
      const description = s.description ? String(s.description) : ''
      return { key: String(s.name), label: String(s.name), terms: description,
        hint: description.length > 80 ? description.slice(0, 79) + '…' : description, group: s.category || undefined }
    }).sort(function (a, b) {
      return String(a.group || '').localeCompare(String(b.group || '')) || a.label.localeCompare(b.label)
    })
  }

  function labelFrom(options, key, fallback) {
    for (let i = 0; i < options.length; i++) if (String(options[i].key) === String(key)) return options[i].label
    return fallback == null ? String(key) : fallback
  }

  // ── page chrome ───────────────────────────────────────────────────────────────────────────────────

  function modeBadge(meta, directoryAt, readOnly) {
    const mode = readOnly ? 'read-only' : meta && meta.mode
    if (mode === 'live') return '● live'
    if (mode === 'snapshot') return 'ⓘ ' + (directoryAt != null ? 'directory of ' + fmtShort(directoryAt) : 'directory')
    if (mode === 'read-only') return '⚠ read-only'
    return 'mode unknown'
  }

  function PageHeader(props) {
    const meta = props.meta
    return h('div', { style: S.header },
      h('h2', { style: S.h2 }, props.title),
      h('div', { style: S.row },
        h('span', { style: S.secondary }, 'profile: ', h('strong', null, meta && meta.profile ? meta.profile : '…')),
        h(K.Badge, { variant: 'muted' }, modeBadge(meta, props.directoryAt, props.readOnly))))
  }

  function Loading(props) {
    return h('p', { role: 'status', style: S.small }, props.text || 'Loading…')
  }

  function LoadError(props) {
    const r = props.error
    const notFound = r && r.status === 404
    // The plugin's own 404 carries {error: {code}}; Hermes answers {"detail": …} when the backend is not mounted.
    const backendMissing = (notFound && !r.code) || (r && r.transport)
    return h('div', { role: 'alert', style: S.card },
      h('p', { style: S.error }, '⚠ ' + (props.title || 'The plugin backend is not reachable.')),
      h('p', { style: S.small }, errorText(r)),
      h('p', { style: S.small }, backendMissing
        ? 'Next step: enable the plugin for this profile (Capabilities → Plugins) and restart the gateway, then try again.'
        : notFound ? 'Next step: ' + (props.notFoundStep || 'go back and choose another entry.')
          : 'Next step: try again; if it keeps failing, check the gateway log.'),
      h('div', null, h(Btn, { onClick: props.onRetry }, 'Try again')))
  }

  function Page(props) {
    return h('div', { style: S.page }, h(PageHeader, props), props.children)
  }

  function storeBroken(ov) {
    const state = ov && ov.store && ov.store.state
    return state === 'corrupt' || state === 'downgrade'
  }

  function StoreProblem(props) {
    const store = props.store || {}
    const reason = store.state === 'downgrade'
      ? 'The policy file was written by a newer version of this plugin'
      : 'The policy file is damaged and cannot be read'
    return h('div', { role: 'alert', style: S.card },
      h('p', { style: S.error }, '⚠ ' + reason + (store.error ? ': ' + store.error : '.')),
      h('p', { style: S.p }, 'Meanwhile every server is paused: the bot answers nowhere under this policy. This page ' +
        'shows what it knows but cannot change anything until the file is ' +
        (store.state === 'downgrade' ? 'read by an up-to-date plugin.' : 'repaired or restored from a backup.')),
      Array.isArray(store.problems) && store.problems.length
        ? h('ul', { style: S.list }, store.problems.map(function (p, i) { return h('li', { key: i, style: S.small }, '• ' + p) }))
        : null)
  }

  // ── conflict and offline notices ──────────────────────────────────────────────────────────────────

  function ConflictBanner(props) {
    const c = props.conflict || {}
    const nameOf = useUserNames(props.bot, c.by ? [c.by] : [])
    const via = viaText(c.via)
    const text = '⚠ Not saved: ' + whoText({ by: c.by, label: c.label }, nameOf) + ' changed ' +
      (props.subject || "this server's settings") + ' ' + (c.at != null ? relTime(c.at) : 'recently') +
      (via ? ' (' + via + ')' : '') + ', after you opened this page.'
    return h('div', { role: 'alert', style: S.banner },
      h('span', null, text + ' '),
      h(Btn, { onClick: props.onReload }, 'Reload'),
      h('span', null, ' — your unsaved edits stay in the form until you reload.'))
  }

  function OfflineBanner(props) {
    const dir = props.directory
    const online = props.online
    let text = null
    if (online === false) {
      if (dir && dir.at != null && dir.source !== 'none') {
        text = 'The bot is offline. Suggestions come from the directory of ' + fmtUTC(dir.at) +
          '; channels created since then are missing. You can paste an ID instead.'
      } else {
        text = 'The bot is offline and no directory has been written for this server yet, so there are no ' +
          'suggestions. You can paste an ID instead.'
      }
    } else if (dir && dir.stale && dir.at != null) {
      text = 'Suggestions come from the directory of ' + fmtUTC(dir.at) +
        '; channels created since then are missing. You can paste an ID instead.'
    }
    return text ? h(Banner, null, 'ⓘ ' + text) : null
  }

  // ── Overview ──────────────────────────────────────────────────────────────────────────────────────

  function stateLabel(row) {
    if (row.state === 'active') return '● active'
    if (row.state === 'paused') return '◐ paused'
    return '○ not active'
  }

  function guildSummary(row, nameOf) {
    const s = row.summary || {}
    if (row.state === 'paused') {
      const who = row.updated ? whoText(row.updated, nameOf) : 'someone'
      return 'paused by ' + who + (row.updated && row.updated.at != null ? ' · ' + relTime(row.updated.at) : '')
    }
    if (row.state !== 'active') return row.configured ? 'unbound · settings kept' : 'the bot is in this server · not set up'
    const parts = ['ambient in ' + fmtN(s.ambient || 0)]
    if (s.excluded) parts.push(fmtN(s.excluded) + ' excluded')
    parts.push('guidance ' + fmtN(s.guidance_chars || 0) + '/' + fmtN(s.guidance_max || 0))
    if (s.worklog) parts.push('worklog')
    return parts.join(' · ')
  }

  function directoryText(dir) {
    if (!dir || dir.source === 'none') return 'no directory yet'
    if (dir.source === 'live') return 'directory live'
    if (dir.source === 'rpc') return 'directory live (through the gateway)'
    return 'directory of ' + fmtShort(dir.at) + (dir.stale ? ' (old)' : '')
  }

  function newestDirectory(ov) {
    let best = null
    ;((ov && ov.bots) || []).forEach(function (b) {
      const at = b.directory ? toMs(b.directory.at) : null
      if (at != null && (best == null || at > best)) best = at
    })
    return best
  }

  function BotRow(props) {
    const b = props.bot
    const foreign = b.owner_profile && props.profile && b.owner_profile !== props.profile
    return h('li', { style: { padding: '4px 0' } },
      h('div', { style: S.row },
        h('strong', null, b.name || 'Bot ' + shortId(b.id)),
        h('span', { style: S.small }, '(application ' + shortId(b.id) + ')'),
        h('span', null, b.online ? '● online' : '○ offline'),
        h('span', { style: S.secondary }, '· ' + directoryText(b.directory))),
      foreign ? h('p', { style: S.small }, 'This bot belongs to profile ' + b.owner_profile + '. Next step: switch the ' +
        'active profile to ' + b.owner_profile + ' to configure it.') : null)
  }

  function Protection(props) {
    const p = props.protection || {}
    const armed = Array.isArray(p.armed_profiles) ? p.armed_profiles : []
    const degraded = Array.isArray(p.security_degraded) ? p.security_degraded : []
    const lines = []
    if (p.status_at == null) {
      lines.push('ⓘ No protection status yet: the gateway writes it when it starts with this plugin enabled.')
    } else {
      lines.push((armed.length ? '✓ armed in ' + plural(armed.length, 'profile', 'profiles') + ' (' + armed.join(', ') + ')'
        : '⚠ not armed in any profile') + ' · comfort: ' + plural(p.comfort_fallbacks || 0, 'fallback', 'fallbacks'))
      if (degraded.length) {
        lines.push('⚠ ' + plural(degraded.length, 'security item', 'security items') + ' degraded: ' +
          degraded.map(function (d) { return d.id + ' (' + d.outcome + (d.reason ? ': ' + d.reason : '') + ')' }).join(', '))
      }
    }
    return h(K.ListRow, {
      title: 'Protection',
      description: h('span', null, lines.map(function (l, i) { return h('span', { key: i, style: { display: 'block' } }, l) })),
      action: h(Btn, { onClick: function () { nav.go('status') } }, 'View status')
    })
  }

  function OperatorsSummary(props) {
    const o = props.operators || {}
    const text = o.readable === false
      ? '⚠ The Hermes configuration could not be read; nobody counts as an operator until it is fixed.'
      : (o.count ? plural(o.count, 'operator', 'operators') + ' configured'
        : 'No operator configured: only public lanes; activation only from this page or the CLI.')
    return h(K.ListRow, { title: 'Operators', description: text,
      action: h(Btn, { onClick: function () { nav.go('operators') } }, 'Edit') })
  }

  function ServerRow(props) {
    const row = props.row
    const [confirm, setConfirm] = useState(false)
    const [busy, setBusy] = useState(false)
    const [status, setStatus] = useState(null)
    const canActivate = !props.readOnly && row.state !== 'active' && row.state !== 'paused'
    const name = row.name || 'Server ' + row.id
    function activate() {
      setBusy(true)
      call(gpath(row.bot, row.id) + '/state', { method: 'POST', body: { expected_rev: row.rev || 0, state: 'active' } })
        .then(function (r) {
          setBusy(false)
          setConfirm(false)
          if (r.ok) {
            setStatus('✓ Activated in ' + name + '.')
            props.onChanged()
          } else {
            setStatus('✕ Not activated: ' + errorText(r))
          }
        })
    }
    return h('li', { style: { padding: '6px 0', borderBottom: '1px solid ' + V.stroke } },
      h('div', { style: S.row },
        h('span', { style: { minWidth: '7em' } }, stateLabel(row)),
        h('button', { type: 'button', style: Object.assign({}, S.linkButton, { fontWeight: 600 }),
          onClick: function () { openServer(row.bot, row.id) } }, name),
        row.configured ? h('span', { style: S.small }, 'rev ' + fmtN(row.rev || 0)) : null,
        h('span', { style: S.secondary }, guildSummary(row, props.nameOf)),
        canActivate && !confirm ? h(Btn, { onClick: function () { setConfirm(true) } }, 'Activate…') : null),
      confirm ? h('div', { role: 'group', 'aria-label': 'Confirm activation', style: Object.assign({}, S.banner, { marginTop: '6px' }) },
        h('span', null, 'Activate the policy in ' + name + '? With the defaults the bot answers only when addressed ' +
          'and listens in no channel; you can change that afterwards.'),
        h(Btn, { variant: 'default', disabled: busy, onClick: activate }, 'Activate'),
        h(Btn, { variant: 'text', onClick: function () { setConfirm(false) } }, 'Cancel')) : null,
      status ? h('p', { role: 'status', style: S.small }, status,
        status.charAt(0) === '✓' ? h('span', null, ' ', h('button', { type: 'button', style: S.linkButton,
          onClick: function () { openServer(row.bot, row.id) } }, 'Open its settings')) : null) : null)
  }

  function Overview() {
    const meta = useLoad('/v1/meta')
    const ov = useLoad('/v1/overview')
    const [search, setSearch] = useState('')
    const data = ov.data
    const bots = (data && data.bots) || []
    const rows = []
    bots.forEach(function (b) {
      ;(b.guilds || []).forEach(function (g) { rows.push(Object.assign({ bot: String(b.id), botName: b.name }, g)) })
    })
    const nameIds = rows.map(function (r) { return r.updated && r.updated.by }).filter(Boolean)
    const nameOf = useUserNames(bots.length ? String(bots[0].id) : null, nameIds)
    const broken = storeBroken(data)
    const readOnly = broken || (meta.data && meta.data.mode === 'read-only') || (data && data.mode === 'read-only')
    const q = search.trim().toLowerCase()
    const visible = rows.filter(function (r) {
      return !q || String(r.name || '').toLowerCase().indexOf(q) >= 0 || String(r.id).indexOf(q) >= 0
    })

    const header = { title: 'ctx_discord_policy', meta: meta.data || (data ? { profile: data.profile, mode: data.mode } : null),
      directoryAt: newestDirectory(data), readOnly: readOnly }
    if (!data && ov.error) {
      return h(Page, header, h(LoadError, { error: ov.error, onRetry: function () { meta.reload(); ov.reload() } }))
    }
    if (!data) return h(Page, header, h(Loading))

    const parts = []
    if (meta.data && meta.data.conflict) parts.push(h(Banner, { key: 'conflict', role: 'alert' }, '⚠ ' + meta.data.conflict))
    if (broken) parts.push(h(StoreProblem, { key: 'store', store: data.store }))

    if (!bots.length) {
      parts.push(h('div', { key: 'empty', style: S.card },
        h(K.EmptyState, { title: 'No bot yet',
          description: 'The bot appears here once the Hermes gateway has connected it to Discord. You can already ' +
            'set the operators now.' }),
        data.next_step ? h('p', { style: Object.assign({}, S.small, { textAlign: 'center' }) }, 'Next step: ' + data.next_step) : null,
        h('div', { style: { display: 'flex', justifyContent: 'center' } },
          h(Btn, { variant: 'default', onClick: function () { nav.go('operators') } }, 'Go to Operators'))))
    } else {
      parts.push(h('section', { key: 'bots', 'aria-labelledby': 'dsp-ov-bots', style: S.card },
        h('h3', { id: 'dsp-ov-bots', style: S.h3 }, bots.length === 1 ? 'Bot' : 'Bots'),
        h('ul', { style: S.list }, bots.map(function (b) { return h(BotRow, { key: b.id, bot: b, profile: data.profile }) }))))
    }
    ;(data.other_profiles || []).forEach(function (o) {
      parts.push(h('p', { key: 'other-' + o.profile, style: S.small }, 'ⓘ Profile ' + o.profile + ' has ' +
        plural((o.bots || []).length, 'bot', 'bots') + '. Next step: switch the active profile to ' + o.profile +
        ' to configure ' + ((o.bots || []).length === 1 ? 'it.' : 'them.')))
    })
    bots.forEach(function (b) {
      if (b.online === false) parts.push(h(OfflineBanner, { key: 'off-' + b.id, online: false, directory: b.directory }))
    })
    parts.push(h('div', { key: 'summary', style: S.card },
      h(Protection, { protection: data.protection }),
      h(OperatorsSummary, { operators: data.operators })))

    if (bots.length) {
      parts.push(h('section', { key: 'servers', 'aria-labelledby': 'dsp-ov-servers', style: S.card },
        h('div', { style: Object.assign({}, S.row, { justifyContent: 'space-between' }) },
          h('h3', { id: 'dsp-ov-servers', style: S.h3 }, 'Servers (' + rows.length + ')'),
          h('div', { style: { flex: '0 1 18em' } }, h(TextInput, { type: 'search', 'aria-label': 'Search servers',
            placeholder: 'Search servers…', value: search, onChange: function (e) { setSearch(e.target.value) } }))),
        rows.length === 0
          ? h('p', { style: S.small }, 'The bot is in no server yet. Invite it to a server; it appears here once ' +
            'Discord reports it.')
          : h('ul', { style: S.list }, visible.map(function (r) {
            return h(ServerRow, { key: r.bot + '/' + r.id, row: r, readOnly: readOnly, nameOf: nameOf,
              onChanged: function () { ov.reload() } })
          })),
        rows.length && !visible.length ? h('p', { role: 'status', style: S.small }, 'No server matches “' + search + '”.') : null))
    }
    return h(Page, header, parts)
  }

  // ── Servers: field controls ───────────────────────────────────────────────────────────────────────

  function isPatchable(field) { return field.kind !== 'guidance' && field.kind !== 'readonly' }
  function isRoutingSection(section) {
    return section.key === 'profile' || section.key === 'model' || section.key === 'routing' ||
      (section.fields || []).some(function (f) { return /^(routing|models)\./.test(String(f.path)) })
  }

  // What a read-only profile or model section says about the current choice.
  function routingSummary(section, settings) {
    const routing = (settings && settings.routing) || {}
    const models = (settings && settings.models) || {}
    const modelText = function (m) { return m && m.model ? (m.provider ? m.provider + ' · ' : '') + m.model : null }
    if ((section.fields || []).some(function (f) { return String(f.path).indexOf('routing.') === 0 })) {
      return routing.profile ? 'Runs in the Hermes profile ' + routing.profile + '.' : 'Runs in the bot’s own profile.'
    }
    const parts = []
    if (modelText(models.public)) parts.push('Public lanes: ' + modelText(models.public) + '.')
    if (modelText(models.operator)) parts.push('Operator turns: ' + modelText(models.operator) + '.')
    if (models.manager_pick && modelText(models.manager_pick)) parts.push('Picked by the managers: ' + modelText(models.manager_pick) + '.')
    return parts.length ? parts.join(' ') : 'The profile’s own model.'
  }

  function FieldMessages(props) {
    const out = []
    if (props.error) out.push(h('p', { key: 'e', id: props.errorId, style: S.error }, '✕ ' + props.error))
    ;(props.warnings || []).forEach(function (w, i) { out.push(h('p', { key: 'w' + i, style: S.warning }, '⚠ ' + w)) })
    return out.length ? h('div', { style: { display: 'grid', gap: '2px' } }, out) : null
  }

  function FieldRow(props) {
    const title = h('span', { id: props.labelId }, props.label)
    const messages = h(FieldMessages, { error: props.error, errorId: props.errorId, warnings: props.warnings })
    if (props.wide) {
      return h(K.ListRow, { title: title, description: props.help || undefined, wide: true,
        action: props.counter ? h('span', { id: props.counterId, style: S.small }, props.counter) : undefined,
        below: h('div', { style: { display: 'grid', gap: '4px', marginTop: '6px' } }, props.control, messages) })
    }
    return h(K.ListRow, { title: title, description: props.help || undefined,
      action: h('div', { style: Object.assign({}, S.row, { flexWrap: 'nowrap' }) }, props.control,
        props.counter ? h('span', { id: props.counterId, style: Object.assign({}, S.small, { whiteSpace: 'nowrap' }) }, props.counter) : null),
      below: messages })
  }

  function unknownIdsNote(keys, options, what) {
    const known = new Set(options.map(function (o) { return String(o.key) }))
    const unknown = keys.filter(function (k) { return !known.has(String(k)) })
    if (!unknown.length || !options.length) return []
    return [plural(unknown.length, 'ID is', 'IDs are') + ' not in the directory (' + unknown.join(', ') + '); that is fine if the ' +
      what + ' is new.']
  }

  function ListField(props) {
    const f = props.field
    const value = props.value
    const opts = props.options
    const single = props.single
    const keys = single ? (value == null ? [] : [String(value)]) : (Array.isArray(value) ? value.map(String) : [])
    const selected = keys.map(function (k) { return { key: k, label: labelFrom(opts, k) } })
    const known = new Set(opts.map(function (o) { return String(o.key) }))
    return h(Combobox, {
      id: props.fid, labelledBy: props.labelId, selected: selected, options: opts, single: single,
      limit: f.max_items, noun: props.noun, placeholder: props.placeholder, describedBy: props.describedBy,
      invalid: props.invalid, disabled: props.disabled,
      freeOptions: function (text, chosen) {
        if (props.freeSkill) {
          if (!SKILL_RE.test(text) || chosen.has(text) || known.has(text)) return []
          return [{ key: text, label: 'Use skill “' + text + '”', free: true }]
        }
        return idOption(text, chosen, known)
      },
      onAdd: function (o) { props.onChange(single ? String(o.key) : keys.concat([String(o.key)])) },
      onRemove: function (k) { props.onChange(single ? null : keys.filter(function (x) { return x !== String(k) })) }
    })
  }

  function EmojiField(props) {
    const opts = props.options
    const v = props.value
    const selected = v == null ? [] : [typeof v === 'string' ? { key: 'u:' + v, label: v }
      : { key: String(v.id), label: ':' + (v.name || v.id) + ':' }]
    return h(Combobox, {
      id: props.fid, labelledBy: props.labelId, selected: selected, options: opts, single: true,
      noun: ['emoji', 'emojis'], placeholder: 'search server emojis or type a unicode emoji…',
      describedBy: props.describedBy, invalid: props.invalid, disabled: props.disabled,
      freeOptions: function (text) {
        if (/^[\x20-\x7e]*$/.test(text) || text.length > 32) return []
        return [{ key: 'u:' + text, label: 'Use ' + text, value: text, free: true }]
      },
      onAdd: function (o) { props.onChange(o.value !== undefined ? o.value : String(o.key)) },
      onRemove: function () { props.onChange(null) }
    })
  }

  function TextListField(props) {
    const f = props.field
    const items = Array.isArray(props.value) ? props.value.map(String) : []
    const max = f.max_length
    return h(Combobox, {
      id: props.fid, labelledBy: props.labelId, selected: items.map(function (t) { return { key: t, label: t } }),
      options: [], limit: f.max_items, noun: ['entry', 'entries'], placeholder: 'add…',
      describedBy: props.describedBy, invalid: props.invalid, disabled: props.disabled,
      liveText: ' ',
      freeOptions: function (text, chosen) {
        if (chosen.has(text) || (max && text.length > max)) return []
        return [{ key: text, label: 'Add “' + text + '”', free: true }]
      },
      onAdd: function (o) { props.onChange(items.concat([String(o.key)])) },
      onRemove: function (k) { props.onChange(items.filter(function (x) { return x !== String(k) })) }
    })
  }

  function KnowledgeField(props) {
    const f = props.field
    const items = Array.isArray(props.value) ? props.value : []
    const [tool, setTool] = useState('')
    const [ident, setIdent] = useState('')
    const [problem, setProblem] = useState(null)
    const full = f.max_items != null && items.length >= f.max_items
    const addErrId = props.fid + '-add-error'
    function add() {
      const t = tool.trim()
      const i = ident.trim()
      if (!TOOL_RE.test(t)) {
        setProblem('The tool must be an MCP read tool, for example mcp__server__get_page.')
        return
      }
      if (!i || i.length > 200) {
        setProblem('Enter an id of at most 200 characters.')
        return
      }
      if (items.some(function (x) { return x.tool === t && x.id === i })) {
        setProblem('This reference is already in the list.')
        return
      }
      setProblem(null)
      setTool('')
      setIdent('')
      props.onChange(items.concat([{ tool: t, id: i }]))
    }
    return h('div', { style: { display: 'grid', gap: '6px' } },
      items.length ? h('ul', { style: S.list }, items.map(function (x, n) {
        return h('li', { key: n + ':' + x.tool + ':' + x.id, style: S.row },
          h('code', { style: { fontFamily: V.mono, fontSize: V.small } }, x.tool),
          h('span', null, x.id),
          h(Btn, { variant: 'text', disabled: props.disabled, 'aria-label': 'Remove ' + x.tool + ' ' + x.id,
            onClick: function () { props.onChange(items.filter(function (_, k) { return k !== n })) } }, 'Remove'))
      })) : h('p', { style: S.small }, 'No knowledge references.'),
      h('div', { role: 'group', 'aria-label': 'Add a knowledge reference', style: S.row },
        h(TextInput, { 'aria-label': 'Tool', placeholder: 'tool, e.g. mcp__server__get_page', value: tool,
          disabled: props.disabled || full, style: { flex: '2 1 14em', width: 'auto', fontFamily: V.mono },
          'aria-describedby': problem ? addErrId : undefined, 'aria-invalid': problem ? 'true' : undefined,
          onChange: function (e) { setTool(e.target.value) } }),
        h(TextInput, { 'aria-label': 'Id', placeholder: 'id', value: ident, disabled: props.disabled || full,
          style: { flex: '1 1 8em', width: 'auto' }, 'aria-describedby': problem ? addErrId : undefined,
          onChange: function (e) { setIdent(e.target.value) },
          onKeyDown: function (e) { if (e.key === 'Enter') { e.preventDefault(); add() } } }),
        h(Btn, { disabled: props.disabled || full, onClick: add, 'aria-label': 'Add knowledge reference' }, '+ Add')),
      problem ? h('p', { id: addErrId, style: S.error }, '✕ ' + problem) : null,
      full ? h('p', { style: S.small }, 'Limit of ' + f.max_items + ' reached.') : null)
  }

  function TemplatesField(props) {
    const f = props.field
    const value = props.value && typeof props.value === 'object' ? props.value : {}
    const max = f.max_length || 8000
    const names = []
    ;(Array.isArray(f.choices) ? f.choices.map(function (c) { return String(c.value) }) : DEFAULT_TEMPLATES)
      .concat(Object.keys(value)).forEach(function (n) { if (names.indexOf(n) < 0) names.push(n) })
    const labels = {}
    ;(f.choices || []).forEach(function (c) { labels[String(c.value)] = c.label })
    return h('div', { style: { display: 'grid', gap: '12px' } }, names.map(function (name) {
      const tid = props.fid + '-' + name
      const overridden = hasOwn(value, name)
      const text = overridden ? String(value[name]) : ''
      return h('div', { key: name, style: { display: 'grid', gap: '4px' } },
        h('div', { style: Object.assign({}, S.row, { justifyContent: 'space-between' }) },
          h('label', { htmlFor: tid, style: { fontWeight: 500 } }, (labels[name] || name) +
            (overridden ? ' — replaced' : ' — built-in text')),
          h('span', { style: S.small }, fmtN(text.length) + '/' + fmtN(max))),
        h(TextArea, { id: tid, value: text, rows: 6, disabled: props.disabled, placeholder: '(empty: the built-in text is used)',
          style: { minHeight: '8em' },
          onChange: function (e) {
            const next = Object.assign({}, value)
            if (e.target.value === '') delete next[name]
            else next[name] = e.target.value
            props.onChange(next)
          } }),
        overridden ? h('div', null, h(Btn, { variant: 'text', disabled: props.disabled,
          onClick: function () { props.onChange(omit(value, [name])) } }, 'Use the built-in text')) : null)
    }))
  }

  function choiceId(v) { return String(v) }

  function FieldControl(props) {
    const f = props.field
    const ctx = props.ctx
    const value = ctx.valueOf(f)
    const fid = domId('dsp-field', f.path)
    const labelId = fid + '-label'
    const error = ctx.errorFor(f.path)
    const errorId = error ? fid + '-error' : undefined
    const warnings = ctx.warningsFor(f.path)
    const disabled = ctx.readOnly
    const set = function (v) { ctx.setValue(f, v) }
    const base = { label: f.label || f.path, labelId: labelId, help: f.help, error: error, errorId: errorId }
    const dir = ctx.directory
    let options

    switch (f.kind) {
      case 'channel_list':
      case 'channel': {
        options = channelOptions(dir)
        const keys = f.kind === 'channel' ? (value == null ? [] : [value]) : (value || [])
        return h(FieldRow, Object.assign({}, base, {
          wide: true, counter: f.kind === 'channel_list' && f.max_items ? keys.length + '/' + f.max_items : null,
          counterId: fid + '-count',
          warnings: warnings.concat(unknownIdsNote(keys.map(String), options, 'channel')),
          control: h(ListField, { field: f, fid: fid, labelId: labelId, value: value, options: options,
            single: f.kind === 'channel', noun: ['channel', 'channels'], placeholder: 'search channels or paste an ID…',
            describedBy: joinIds(errorId, fid + '-count'), invalid: !!error, disabled: disabled, onChange: set })
        }))
      }
      case 'role_list':
        options = roleOptions(dir, ctx.guild)
        return h(FieldRow, Object.assign({}, base, {
          wide: true, counter: f.max_items ? (value || []).length + '/' + f.max_items : null, counterId: fid + '-count',
          warnings: warnings.concat(unknownIdsNote((value || []).map(String), options, 'role')),
          control: h(ListField, { field: f, fid: fid, labelId: labelId, value: value, options: options,
            noun: ['role', 'roles'], placeholder: 'search roles or paste an ID…', describedBy: joinIds(errorId, fid + '-count'),
            invalid: !!error, disabled: disabled, onChange: set })
        }))
      case 'skill_list':
        options = skillOptions(ctx.skills)
        return h(FieldRow, Object.assign({}, base, {
          wide: true, counter: f.max_items ? (value || []).length + '/' + f.max_items : null, counterId: fid + '-count',
          warnings: warnings,
          control: h(ListField, { field: f, fid: fid, labelId: labelId, value: value, options: options, freeSkill: true,
            noun: ['skill', 'skills'], placeholder: 'search skills…', describedBy: joinIds(errorId, fid + '-count'),
            invalid: !!error, disabled: disabled, onChange: set })
        }))
      case 'emoji':
        return h(FieldRow, Object.assign({}, base, {
          wide: true, warnings: warnings,
          control: h(EmojiField, { fid: fid, labelId: labelId, value: value, options: emojiOptions(dir),
            describedBy: errorId, invalid: !!error, disabled: disabled, onChange: set })
        }))
      case 'text_list':
        return h(FieldRow, Object.assign({}, base, {
          wide: true, counter: f.max_items ? (value || []).length + '/' + f.max_items : null, counterId: fid + '-count',
          warnings: warnings,
          control: h(TextListField, { field: f, fid: fid, labelId: labelId, value: value,
            describedBy: joinIds(errorId, fid + '-count'), invalid: !!error, disabled: disabled, onChange: set })
        }))
      case 'text': {
        const text = value == null ? '' : String(value)
        return h(FieldRow, Object.assign({}, base, {
          warnings: warnings, counter: f.max_length ? text.length + '/' + f.max_length : null, counterId: fid + '-count',
          control: h(TextInput, { id: fid, value: text, disabled: disabled, 'aria-labelledby': labelId,
            'aria-describedby': joinIds(errorId, f.max_length ? fid + '-count' : null), 'aria-invalid': error ? 'true' : undefined,
            style: { minWidth: '14em' }, onChange: function (e) { set(e.target.value) } })
        }))
      }
      case 'int': {
        const bounds = (f.min != null ? f.min : '…') + '–' + (f.max != null ? f.max : '…')
        return h(FieldRow, Object.assign({}, base, {
          warnings: warnings, counter: bounds, counterId: fid + '-count',
          control: h(TextInput, { id: fid, type: 'number', inputMode: 'numeric', step: 1, min: f.min, max: f.max,
            value: value == null ? '' : String(value), disabled: disabled, 'aria-labelledby': labelId,
            'aria-describedby': joinIds(errorId, fid + '-count'), 'aria-invalid': error ? 'true' : undefined,
            style: { width: '7em' },
            onChange: function (e) {
              const raw = e.target.value
              set(/^-?[0-9]+$/.test(raw) ? Number(raw) : raw)
            } })
        }))
      }
      case 'bool':
        return h(K.ToggleRow, { id: fid, label: f.label || f.path, description: f.help || undefined, checked: !!value,
          disabled: disabled, onChange: function (on) { set(!!on) },
          below: h(FieldMessages, { error: error, errorId: errorId, warnings: warnings }) })
      case 'choice': {
        const choices = Array.isArray(f.choices) ? f.choices : []
        const current = choices.filter(function (c) { return equal(c.value, value) })[0]
        const labelsLength = choices.reduce(function (n, c) { return n + String(c.label).length }, 0)
        const pick = function (idValue) {
          const c = choices.filter(function (x) { return choiceId(x.value) === idValue })[0]
          if (c) set(c.value)
        }
        if (choices.length <= 4 && labelsLength <= 64) {
          return h(FieldRow, Object.assign({}, base, {
            warnings: warnings,
            control: h(Segmented, { id: fid, labelledBy: labelId, describedBy: errorId, disabled: disabled,
              value: current ? choiceId(current.value) : '',
              options: choices.map(function (c) { return { id: choiceId(c.value), label: String(c.label) } }), onChange: pick })
          }))
        }
        return h(FieldRow, Object.assign({}, base, {
          wide: true, warnings: warnings,
          control: h('div', { id: fid, role: 'radiogroup', 'aria-labelledby': labelId, 'aria-describedby': errorId,
            style: { display: 'grid', gap: '2px' } },
            choices.map(function (c) {
              const rid = fid + '-' + domId('c', c.value)
              return h('label', { key: rid, htmlFor: rid, style: S.row },
                h('input', { id: rid, type: 'radio', name: fid, disabled: disabled, checked: !!current && equal(current.value, c.value),
                  onChange: function () { set(c.value) } }), String(c.label))
            }))
        }))
      }
      case 'knowledge_list':
        return h(FieldRow, Object.assign({}, base, {
          wide: true, warnings: warnings, counter: f.max_items ? (value || []).length + '/' + f.max_items : null,
          counterId: fid + '-count',
          control: h('div', { id: fid, role: 'group', 'aria-labelledby': labelId, 'aria-describedby': joinIds(errorId, fid + '-count') },
            h(KnowledgeField, { field: f, fid: fid, value: value, disabled: disabled, onChange: set }))
        }))
      case 'templates':
        return h(FieldRow, Object.assign({}, base, {
          wide: true, warnings: warnings,
          control: h('div', { id: fid, role: 'group', 'aria-labelledby': labelId, 'aria-describedby': errorId },
            h(TemplatesField, { field: f, fid: fid, value: value, disabled: disabled, onChange: set }))
        }))
      case 'guidance':
        return h(GuidanceSummary, { field: f, ctx: ctx })
      default:
        return h(FieldRow, Object.assign({}, base, {
          warnings: warnings,
          control: h('span', { id: fid, style: S.secondary }, value == null || value === ''
            ? '—' : (typeof value === 'object' ? JSON.stringify(value) : String(value)))
        }))
    }
  }

  function GuidanceSummary(props) {
    const ctx = props.ctx
    const g = (ctx.base && ctx.base.guidance) || {}
    const history = Array.isArray(g.history) ? g.history : []
    const check = ctx.lastCheck
    const text = String(getPath(ctx.base && ctx.base.settings, props.field.path) || '')
    let line = fmtN(g.chars != null ? g.chars : text.length) + ' / ' + fmtN(g.max_chars) + ' chars'
    if (check) line += ' · needs ' + fmtN(check.needed) + ' · room ' + (check.room == null ? 'unlimited' : fmtN(check.room))
    return h(K.ListRow, {
      title: props.field.label || 'Guidance',
      description: h('span', null, props.field.help ? h('span', { style: { display: 'block' } }, props.field.help) : null,
        h('span', null, line)),
      wide: true,
      below: h('div', { style: Object.assign({}, S.row, { marginTop: '6px' }) },
        h(Btn, { onClick: function () { ctx.openGuidance('edit') } }, 'Edit guidance'),
        h(Btn, { onClick: function () { ctx.openGuidance('preview') } }, 'Preview'),
        h(Btn, { onClick: function () { ctx.openGuidance('history') } }, 'History (' + history.length + ')'),
        h(Btn, { onClick: function () { download('guidance-' + ctx.guild + '.md', text, 'text/markdown') } }, 'Download'))
    })
  }

  // ── Servers: sections and the detail ──────────────────────────────────────────────────────────────

  function localProblems(section, changes) {
    const out = []
    section.fields.forEach(function (f) {
      if (!hasOwn(changes, f.path)) return
      const v = changes[f.path]
      if (f.kind === 'int') {
        if (typeof v !== 'number' || !Number.isInteger(v) || (f.min != null && v < f.min) || (f.max != null && v > f.max)) {
          out.push({ path: f.path, text: 'Enter a whole number between ' + (f.min != null ? f.min : '…') + ' and ' +
            (f.max != null ? f.max : '…') + '.' })
        }
      } else if (f.kind === 'text' && f.max_length && String(v || '').length > f.max_length) {
        out.push({ path: f.path, text: 'At most ' + f.max_length + ' characters.' })
      } else if (Array.isArray(v) && f.max_items != null && v.length > f.max_items) {
        out.push({ path: f.path, text: 'At most ' + f.max_items + ' entries.' })
      }
    })
    return out
  }

  function fieldForPath(sections, path) {
    if (!path) return null
    let best = null
    sections.forEach(function (s) {
      ;(s.fields || []).forEach(function (f) {
        const p = String(f.path)
        if (path === p || path.indexOf(p + '.') === 0 || path.indexOf(p + '[') === 0) {
          if (!best || p.length > best.path.length) best = f
        }
      })
    })
    return best
  }

  function problemsFrom(r, sections) {
    const problems = []
    const warnings = []
    if (r.findings && r.findings.length) {
      r.findings.forEach(function (fd) {
        const f = fieldForPath(sections, fd.field)
        const entry = { path: f ? f.path : null, text: fd.message || fd.code, raw: fd.field }
        if (fd.level === 'warning') warnings.push(entry)
        else problems.push(entry)
      })
    }
    if (!problems.length) {
      const f = fieldForPath(sections, r.field)
      problems.push({ path: f ? f.path : null, text: r.message, raw: r.field })
    }
    return { problems: problems, warnings: warnings }
  }

  function findingsByPath(findings, sections) {
    const out = {}
    ;(findings || []).forEach(function (fd) {
      const f = fieldForPath(sections, fd.field)
      const key = f ? f.path : '*'
      ;(out[key] = out[key] || []).push(fd.message || fd.code)
    })
    return out
  }

  function focusField(path) {
    if (typeof document === 'undefined') return
    const el = document.getElementById(domId('dsp-field', path)) ||
      document.getElementById(domId('dsp-field', path) + '-label')
    if (!el) return
    const target = /^(INPUT|TEXTAREA|BUTTON|SELECT)$/.test(el.tagName) ? el
      : el.querySelector('input, textarea, button, select') ||
        (el.parentElement ? el.parentElement.querySelector('input, textarea, button, select') : null)
    if (target && typeof target.focus === 'function') target.focus()
  }

  function ErrorSummary(props) {
    const items = props.items || []
    if (!items.length) return null
    return h('div', { role: 'alert', style: Object.assign({}, S.banner, { display: 'grid' }) },
      h('p', { style: S.error }, '✕ Not saved — ' + plural(items.length, 'problem', 'problems') + ':'),
      h('ul', { style: S.list }, items.map(function (it, i) {
        const f = it.path ? fieldForPath(props.sections, it.path) : null
        const label = f ? (f.label || f.path) : (it.raw || 'Request')
        return h('li', { key: i, style: S.small },
          f ? h('button', { type: 'button', style: S.linkButton, onClick: function () { focusField(f.path) } }, label)
            : h('span', null, label), ': ' + it.text)
      })))
  }

  function SectionView(props) {
    const section = props.section
    const ctx = props.ctx
    const st = ctx.sectionState[section.key] || {}
    const headId = domId('dsp-section', section.key)
    const statusId = headId + '-status'
    const [confirmReset, setConfirmReset] = useState(false)
    const [expanded, setExpanded] = useState(false)
    const patchable = (section.fields || []).filter(isPatchable)
    const dirty = patchable.some(function (f) { return ctx.isDirty(f.path) })
    const busy = ctx.busy === section.key
    const routing = isRoutingSection(section)
    const routingOff = routing && (!(ctx.features && ctx.features.routing_ops) || ctx.routingUnavailable)
    let statusText = ''
    if (st.status && st.status.error) statusText = st.status.text
    else if (dirty) statusText = '● Unsaved changes'
    else if (st.status) statusText = st.status.text
    const body = routingOff
      ? [h('p', { key: 'na', style: S.secondary }, (section.fields || []).length
          ? routingSummary(section, ctx.base && ctx.base.settings) + ' Chosen in Discord for now: /policy, Profile and Model.'
          : 'ⓘ Not available in this version: choosing a profile per server needs a later version of the plugin.')]
      : (section.fields || []).map(function (f) { return h(FieldControl, { key: f.path, field: f, ctx: ctx }) })
    const footer = h('div', { key: 'footer', style: Object.assign({}, S.row, { justifyContent: 'space-between', marginTop: '4px' }) },
      h('p', { id: statusId, role: 'status', tabIndex: -1, ref: function (el) { ctx.statusRefs.current[section.key] = el },
        style: Object.assign({}, S.small, { outline: 'none' }) }, statusText),
      routingOff || ctx.readOnly ? null : h('div', { style: S.rowEnd },
        resetUnavailable ? h('span', { style: S.small }, 'Reset is not available in this version.') : null,
        confirmReset ? h('span', { role: 'group', 'aria-label': 'Confirm reset', style: S.row },
          h('span', null, 'Reset ' + section.label + ' to the defaults?'),
          h(Btn, { variant: 'destructive', disabled: busy, onClick: function () { setConfirmReset(false); ctx.resetSection(section) } }, 'Reset'),
          h(Btn, { variant: 'text', onClick: function () { setConfirmReset(false) } }, 'Cancel'))
          : h(Btn, { variant: 'text', disabled: busy || resetUnavailable,
            title: resetUnavailable ? 'Resetting a section is not available in this version.' : undefined,
            onClick: function () { setConfirmReset(true) } }, 'Reset section'),
        patchable.length ? h(Btn, { variant: 'default', disabled: !dirty || busy, 'aria-describedby': statusId,
          onClick: function () { routing ? ctx.saveRouting(section) : ctx.saveSection(section) } }, busy ? 'Saving…' : 'Save') : null))
    const content = [
      section.note ? h('p', { key: 'note', style: S.small }, section.note) : null,
      h('div', { key: 'body' }, body),
      st.summary && st.summary.length ? h(ErrorSummary, { key: 'sum', items: st.summary, sections: ctx.sections }) : null,
      footer
    ]
    if (section.key === 'templates') {
      return h('details', { style: S.details, open: expanded || !!(st.summary && st.summary.length),
        onToggle: function (e) { setExpanded(!!e.currentTarget.open) } },
        h('summary', { style: S.summary }, 'Advanced: template overrides'),
        h('section', { 'aria-labelledby': headId, style: { display: 'grid', gap: '8px', marginTop: '8px' } },
          h('h3', { id: headId, style: S.h3 }, section.label), content))
    }
    return h('section', { 'aria-labelledby': headId, style: S.card }, h('h3', { id: headId, style: S.h3 }, section.label), content)
  }

  function Lifecycle(props) {
    const d = props.detail
    const ctx = props.ctx
    const [confirm, setConfirm] = useState(null)
    const [typed, setTyped] = useState('')
    const name = d.name || d.guild
    const busy = ctx.busy === 'lifecycle'
    const ro = ctx.readOnly
    const confirmName = d.name || String(d.guild)
    const updated = d.updated || {}
    const nameOf = ctx.nameOf
    const actions = []
    if (!ro) {
      if (d.state === 'active') actions.push(h(Btn, { key: 'pause', disabled: busy, onClick: function () { ctx.changeState('paused') } }, 'Pause'))
      else if (d.state === 'paused') actions.push(h(Btn, { key: 'resume', variant: 'default', disabled: busy, onClick: function () { ctx.changeState('active') } }, 'Resume'))
      else actions.push(h(Btn, { key: 'activate', variant: 'default', disabled: busy, onClick: function () { ctx.changeState('active') } }, 'Activate'))
    }
    const changed = d.configured && updated.at != null
      ? 'Changed by ' + whoText(updated, nameOf) + ' ' + relTime(updated.at) + (updated.via ? ' (' + viaText(updated.via) + ')' : '')
      : (d.configured ? 'No changes recorded.' : 'Not active: the bot is in this server, but the policy is not switched on.')
    return h('div', { style: { display: 'grid', gap: '8px' } },
      h('div', { style: Object.assign({}, S.row, { justifyContent: 'space-between' }) },
        h('span', { style: S.secondary }, changed), h('div', { style: S.rowEnd }, actions)),
      h('p', { id: 'dsp-lifecycle-status', role: 'status', tabIndex: -1, ref: function (el) { ctx.statusRefs.current.lifecycle = el },
        style: Object.assign({}, S.small, { outline: 'none' }) }, ctx.lifecycleStatus || ''),
      d.configured && !ro ? h('details', { style: S.details },
        h('summary', { style: S.summary }, 'Danger zone: unbind, delete settings'),
        h('div', { style: { display: 'grid', gap: '8px', marginTop: '8px' } },
          confirm === 'unbind' ? h('div', { role: 'group', 'aria-label': 'Confirm unbinding', style: S.banner },
            h('span', null, 'Unbind ' + name + '? The policy stops applying there; the settings are kept and the server can ' +
              'be activated again.'),
            h(Btn, { variant: 'destructive', disabled: busy, onClick: function () { setConfirm(null); ctx.changeState('unbound') } }, 'Unbind'),
            h(Btn, { variant: 'text', onClick: function () { setConfirm(null) } }, 'Cancel')) : null,
          confirm === 'purge' ? h('div', { role: 'group', 'aria-label': 'Confirm deleting the settings', style: Object.assign({}, S.banner, { display: 'grid' }) },
            h('label', { htmlFor: 'dsp-purge-name' }, 'This removes every setting of ' + name + ' and unbinds the bot there. ' +
              'Type the server name (' + confirmName + ') to confirm:'),
            h(TextInput, { id: 'dsp-purge-name', value: typed, onChange: function (e) { setTyped(e.target.value) } }),
            h('div', { style: S.row },
              h(Btn, { variant: 'destructive', disabled: busy || typed.trim() !== confirmName,
                onClick: function () { setConfirm(null); setTyped(''); ctx.changeState('unbound', true) } }, 'Delete settings'),
              h(Btn, { variant: 'text', onClick: function () { setConfirm(null); setTyped('') } }, 'Cancel'))) : null,
          confirm ? null : h('div', { style: S.row },
            d.state !== 'unbound' ? h(Btn, { onClick: function () { setConfirm('unbind') } }, 'Unbind…') : null,
            h(Btn, { variant: 'destructive', onClick: function () { setConfirm('purge') } }, 'Delete settings…')))) : null)
  }

  function SettingsTransfer(props) {
    const ctx = props.ctx
    const d = props.detail
    const input = useRef(null)
    const busy = ctx.busy === 'transfer'
    const canImport = !ctx.readOnly
    return h('details', { style: S.details },
      h('summary', { style: S.summary }, 'Settings file: download or import'),
      h('div', { style: { display: 'grid', gap: '8px', marginTop: '8px' } },
        h('p', { style: S.small }, 'The file holds this server’s settings and guidance text (no members, no tokens). ' +
          'Importing replaces the settings; whether the policy is active and the guidance history stay as they are.'),
        h('div', { style: S.row },
          d.configured ? h(Btn, { disabled: busy, onClick: ctx.exportSettings }, 'Download settings (.json)') : null,
          canImport ? h('input', { ref: input, type: 'file', accept: '.json,application/json', hidden: true,
            'aria-hidden': 'true', tabIndex: -1, onChange: function (e) {
              const file = e.target.files && e.target.files[0]
              e.target.value = ''
              if (file) ctx.importSettings(file)
            } }) : null,
          canImport ? h(Btn, { disabled: busy || ctx.dirty,
            onClick: function () { if (input.current) input.current.click() } }, 'Import settings…') : null),
        canImport && ctx.dirty ? h('p', { style: S.small }, 'Save or discard your unsaved changes before importing.') : null,
        h('p', { id: 'dsp-transfer-status', role: 'status', tabIndex: -1,
          ref: function (el) { ctx.statusRefs.current.transfer = el },
          style: Object.assign({}, S.small, { outline: 'none' }) }, ctx.transferStatus || '')))
  }

  function ServerDetail(props) {
    const bot = props.bot
    const guild = props.guild
    const path = gpath(bot, guild)
    const g = useLoad(path)
    const dirL = useLoad('/v1/directory/' + enc(bot) + '/' + enc(guild))
    const schemaL = useLoad('/v1/schema', { poll: false })
    const [base, setBase] = useState(null)
    const [edits, setEdits] = useState({})
    const [conflict, setConflict] = useState(null)
    const [sectionState, setSectionState] = useState({})
    const [busy, setBusy] = useState(null)
    const [view, setView] = useState(null)
    const [lifecycleStatus, setLifecycleStatus] = useState(null)
    const [transferStatus, setTransferStatus] = useState(null)
    const [gone, setGone] = useState(false)
    const [locked, setLocked] = useState(null)
    const [routingUnavailable, setRoutingUnavailable] = useState(false)
    const [lastCheck, setLastCheck] = useState(null)
    const [focusTarget, setFocusTarget] = useState(null)
    const statusRefs = useRef({})
    const dirty = Object.keys(edits).length > 0
    const schema = schemaL.data
    const sections = (schema && schema.sections) || []
    const meta = props.meta || {}
    const features = meta.features || {}
    const settings = (base && base.settings) || {}
    const profile = (settings.routing && settings.routing.profile) || meta.profile || ''
    const needSkills = sections.some(function (s) { return (s.fields || []).some(function (f) { return f.kind === 'skill_list' }) })
    const skillsL = useLoad(needSkills && profile ? '/v1/hermes/skills?profile=' + enc(profile) : null, { poll: false })
    const nameOf = useUserNames(bot, base && base.updated && base.updated.by ? [base.updated.by] : [])

    useEffect(function () {
      if (!g.data) return
      if (!base || (!dirty && !view && !conflict)) setBase(g.data)
    }, [g.data])

    useEffect(function () {
      if (!focusTarget) return
      const el = statusRefs.current[focusTarget]
      if (el && typeof el.focus === 'function') el.focus()
      setFocusTarget(null)
    }, [focusTarget])

    const readOnly = !!props.readOnly || !!locked

    function setSec(key, value) {
      setSectionState(function (prev) { const next = Object.assign({}, prev); next[key] = value; return next })
    }

    function valueOf(f) {
      if (hasOwn(edits, f.path)) return edits[f.path]
      const v = getPath(settings, f.path)
      return v === undefined ? f.default : v
    }

    function setValue(f, v) {
      setEdits(function (prev) {
        const next = Object.assign({}, prev)
        const current = getPath(settings, f.path)
        if (equal(v, current === undefined ? f.default : current)) delete next[f.path]
        else next[f.path] = v
        return next
      })
    }

    function isDirty(p) { return hasOwn(edits, p) }

    function errorFor(p) {
      const keys = Object.keys(sectionState)
      for (let i = 0; i < keys.length; i++) {
        const errs = sectionState[keys[i]].errors
        if (errs && errs[p]) return errs[p]
      }
      return null
    }

    function warningsFor(p) {
      let out = []
      Object.keys(sectionState).forEach(function (k) {
        const w = sectionState[k].warnings
        if (w && w[p]) out = out.concat(w[p])
      })
      return out
    }

    function failed(key, r, subject) {
      if (r.status === 409) {
        setConflict(r.conflict || {})
        return { status: { error: true, text: '✕ Not saved: ' + (subject || "this server's settings") +
          ' changed elsewhere; see the notice at the top.' } }
      }
      if (r.status === 422) {
        const found = problemsFrom(r, sections)
        const errors = {}
        found.problems.forEach(function (p) { if (p.path) errors[p.path] = p.text })
        const warnings = {}
        found.warnings.forEach(function (w) { if (w.path) (warnings[w.path] = warnings[w.path] || []).push(w.text) })
        return { errors: errors, warnings: warnings, summary: found.problems,
          status: { error: true, text: '✕ Not saved: ' + plural(found.problems.length, 'problem', 'problems') + ', listed below.' } }
      }
      if (r.status === 423) {
        setLocked(r.message || 'read-only')
        return { status: { error: true, text: '✕ Not saved: ' + errorText(r) + ' This page is read-only until that is fixed.' } }
      }
      if (r.status === 501) return { status: { error: true, text: '✕ Not available in this version: ' + errorText(r) } }
      if (r.status === 503) return { status: { error: true, text: '✕ Not saved: ' + errorText(r) + ' Try again in a moment.' } }
      return { status: { error: true, text: '✕ Not saved: ' + errorText(r) } }
    }

    function applySaved(next, paths) {
      if (next) setBase(next)
      if (paths) setEdits(function (prev) { return omit(prev, paths) })
    }

    function saveSection(section) {
      const paths = (section.fields || []).filter(isPatchable).map(function (f) { return f.path })
      const changes = {}
      paths.forEach(function (p) { if (hasOwn(edits, p)) changes[p] = edits[p] })
      const local = localProblems(section, changes)
      if (local.length) {
        const errors = {}
        local.forEach(function (p) { errors[p.path] = p.text })
        setSec(section.key, { errors: errors, summary: local,
          status: { error: true, text: '✕ Not saved: ' + plural(local.length, 'problem', 'problems') + ', listed below.' } })
        setFocusTarget(section.key)
        return Promise.resolve()
      }
      setBusy(section.key)
      return call(path, { method: 'PATCH', body: { expected_rev: base.rev, section: section.key, changes: changes } })
        .then(function (r) {
          setBusy(null)
          if (r.ok) {
            const next = r.data && r.data.guild ? r.data.guild : null
            const warnings = (r.data && r.data.warnings) || []
            applySaved(next, paths)
            setSec(section.key, { warnings: findingsByPath(warnings, sections),
              status: { text: '✓ Saved' + (next ? ' (rev ' + next.rev + ')' : '') + '.' +
                (warnings.length ? ' ' + plural(warnings.length, 'warning', 'warnings') + ' at the fields.' : '') } })
          } else {
            setSec(section.key, failed(section.key, r))
          }
          setFocusTarget(section.key)
        })
    }

    function saveRouting(section) {
      const f = (section.fields || []).filter(isPatchable)[0]
      if (!f) return Promise.resolve()
      setBusy(section.key)
      return call(path + '/routing', { method: 'POST', body: { expected_rev: base.rev, profile: hasOwn(edits, f.path) ? edits[f.path] : null } })
        .then(function (r) {
          setBusy(null)
          if (r.ok) {
            applySaved(r.data && r.data.guild, [f.path])
            setSec(section.key, { status: { text: '✓ Saved.' } })
          } else {
            if (r.status === 501) setRoutingUnavailable(true)
            setSec(section.key, failed(section.key, r))
          }
          setFocusTarget(section.key)
        })
    }

    function resetSection(section) {
      setBusy(section.key)
      return call(path + '/reset', { method: 'POST', body: { expected_rev: base.rev, section: section.key } })
        .then(function (r) {
          setBusy(null)
          if (r.ok) {
            applySaved(r.data && r.data.guild, (section.fields || []).map(function (f) { return f.path }))
            setSec(section.key, { status: { text: '✓ Reset to the defaults' + (r.data && r.data.guild ? ' (rev ' + r.data.guild.rev + ')' : '') + '.' } })
          } else {
            if (r.status === 501) resetUnavailable = true
            setSec(section.key, r.status === 501
              ? { status: { error: true, text: '✕ Resetting a section is not available in this version.' } }
              : failed(section.key, r))
          }
          setFocusTarget(section.key)
        })
    }

    function changeState(state, purge) {
      setBusy('lifecycle')
      const body = { expected_rev: base.rev || 0, state: state }
      if (purge) body.purge = true
      return call(path + '/state', { method: 'POST', body: body }).then(function (r) {
        setBusy(null)
        if (r.ok) {
          if (r.data && r.data.purged) {
            setLifecycleStatus('✓ Settings deleted.')
            setEdits({})
            g.reload().then(function (rr) {
              if (rr && rr.ok) setBase(rr.data)
              else if (rr && rr.status === 404) setGone(true)     // the bot has left: nothing is left to show
            })
          } else {
            applySaved(r.data && r.data.guild)
            setLifecycleStatus('✓ ' + ({ active: 'Active.', paused: 'Paused.', unbound: 'Unbound; the settings are kept.' }[state] || 'Done.'))
          }
        } else {
          setLifecycleStatus(failed('lifecycle', r).status.text)
        }
        setFocusTarget('lifecycle')
      })
    }

    function exportSettings() {
      setBusy('transfer')
      return call(path + '/export').then(function (r) {
        setBusy(null)
        const name = 'ctx_discord_policy-' + guild + '.json'
        if (!r.ok) setTransferStatus('✕ Not downloaded: ' + errorText(r))
        else if (download(name, JSON.stringify(r.data, null, 2) + '\n', 'application/json')) setTransferStatus('✓ Downloaded ' + name + '.')
        else setTransferStatus('✕ This window cannot save files.')
        setFocusTarget('transfer')
      })
    }

    function importSettings(file) {
      return readJsonFile(file).then(function (read) {
        if (read.error) {
          setTransferStatus('✕ Not imported: ' + read.error)
          setFocusTarget('transfer')
          return null
        }
        setBusy('transfer')
        return call(path + '/import', { method: 'POST', body: { expected_rev: base.rev || 0, document: read.data } })
          .then(function (r) {
            setBusy(null)
            const next = r.ok && r.data ? r.data.guild : null
            const warnings = (r.ok && r.data && r.data.warnings) || []
            if (r.ok && r.data && r.data.changed === false) {
              setTransferStatus('ⓘ Nothing imported: the file holds exactly the settings in effect (rev ' + fmtN(base.rev) + ').')
            } else if (r.ok) {
              if (next) setBase(next)
              setEdits({})
              setSectionState({})
              setTransferStatus('✓ Imported' + (next ? ' (rev ' + next.rev + ')' : '') + '.' + (warnings.length
                ? ' ' + plural(warnings.length, 'warning', 'warnings') + ': ' + warnings.map(function (w) { return w.message }).join(' ')
                : ''))
            } else if (r.status === 409) {
              setConflict(r.conflict || {})
              setTransferStatus("✕ Not imported: this server's settings changed elsewhere; see the notice at the top.")
            } else if (r.status === 422) {
              setTransferStatus('✕ Not imported: ' + errorText(r))
            } else {
              setTransferStatus(failed('transfer', r).status.text.replace('✕ Not saved:', '✕ Not imported:'))
            }
            setFocusTarget('transfer')
          })
      })
    }

    function reloadAll() {
      return g.reload().then(function (r) {
        if (r && r.ok) {
          setBase(r.data)
          setEdits({})
          setConflict(null)
          setSectionState({})
          setLocked(null)
          setView(null)
        }
      })
    }

    if (gone) {
      return h('div', { role: 'status', style: Object.assign({}, S.card, { display: 'grid', gap: '8px' }) },
        h('p', null, '✓ Settings deleted. The bot is no longer in this server, so nothing is left to configure here.'),
        h('div', null, h(Btn, { variant: 'default', onClick: function () { writeSelection(null) } }, 'Choose another server')))
    }
    if (!base && g.error) {
      return h(LoadError, { error: g.error, title: g.error.status === 404 ? 'This server is not known to this profile.' : null,
        notFoundStep: 'choose another server above; a server appears once the bot is in it or it has settings here.',
        onRetry: function () { g.reload() } })
    }
    if (!base || !schema) return h(Loading, { text: schemaL.error ? errorText(schemaL.error) : 'Loading the server…' })

    const directory = dirL.data || base.directory || null
    const ctx = {
      base: base, guild: guild, bot: bot, sections: sections, directory: directory, skills: skillsL.data,
      readOnly: readOnly, features: features, busy: busy, sectionState: sectionState, statusRefs: statusRefs,
      lifecycleStatus: lifecycleStatus, routingUnavailable: routingUnavailable, nameOf: nameOf, lastCheck: lastCheck,
      valueOf: valueOf, setValue: setValue, isDirty: isDirty, errorFor: errorFor, warningsFor: warningsFor,
      saveSection: saveSection, saveRouting: saveRouting, resetSection: resetSection, changeState: changeState,
      openGuidance: function (panel) { setView({ name: 'guidance', panel: panel }) },
      dirty: dirty, transferStatus: transferStatus, exportSettings: exportSettings, importSettings: importSettings
    }
    const title = (base.name || 'Server ' + guild)
    const stateWord = base.state === 'active' ? '● active' : base.state === 'paused' ? '◐ paused' : '○ not active'
    const top = [
      h('div', { key: 'title', style: Object.assign({}, S.row, { justifyContent: 'space-between' }) },
        h('h3', { style: Object.assign({}, S.h3, { fontSize: '0.95rem' }) }, title),
        h('span', { style: S.secondary }, stateWord + (base.configured ? ' · rev ' + fmtN(base.rev) : ''))),
      conflict ? h(ConflictBanner, { key: 'conflict', conflict: conflict, bot: bot, onReload: reloadAll }) : null,
      locked ? h(Banner, { key: 'locked', role: 'alert' }, '⚠ Read-only: ' + locked) : null,
      h(OfflineBanner, { key: 'offline', online: props.online, directory: directory }),
      h('p', { key: 'source', style: S.small }, 'Suggestions: ' + directorySource(directory)),
      h(Lifecycle, { key: 'life', detail: base, ctx: ctx }),
      h(SettingsTransfer, { key: 'transfer', detail: base, ctx: ctx })
    ]
    if (view && view.name === 'guidance') {
      return h('div', { style: { display: 'grid', gap: '12px' } }, top,
        h(GuidanceEditor, { bot: bot, guild: guild, name: title, base: base, schema: schema, features: features,
          readOnly: readOnly, panel: view.panel, conflict: conflict,
          onCheck: setLastCheck,
          onConflict: function (c) { setConflict(c) },
          onSaved: function (next, message) {
            if (next) setBase(next)
            setView(null)
            const gsec = sections.filter(function (s) { return (s.fields || []).some(function (f) { return f.kind === 'guidance' }) })[0]
            if (gsec) {
              setSec(gsec.key, { status: { text: message } })
              setFocusTarget(gsec.key)
            }
          },
          onClose: function () { setView(null) } }))
    }
    const list = sections.slice()
    if (!features.routing_ops && !list.some(isRoutingSection)) {
      const at = list.findIndex(function (s) { return s.key === 'templates' })
      const reserved = { key: 'routing', label: 'Routing', note: 'Which Hermes profile answers in this server, and with which model.', fields: [] }
      if (at >= 0) list.splice(at, 0, reserved)
      else list.push(reserved)
    }
    const body = base.configured === false
      ? [h('p', { key: 'na', style: S.card }, 'ⓘ Activate the policy to configure this server. With the defaults the bot ' +
        'answers only when addressed, listens in no channel and reads only the current channel’s history.')]
      : list.map(function (s) { return h(SectionView, { key: s.key, section: s, ctx: ctx }) })
    return h('div', { style: { display: 'grid', gap: '12px' } }, top, body)
  }

  // ── guidance editor ───────────────────────────────────────────────────────────────────────────────

  function unknownPlaceholders(text, known) {
    const out = []
    const cleaned = String(text).replace(/\{\{|\}\}/g, '')
    let m
    PLACEHOLDER_RE.lastIndex = 0
    m = PLACEHOLDER_RE.exec(cleaned)
    while (m !== null) {
      if (known.indexOf(m[1]) < 0 && out.indexOf(m[1]) < 0) out.push(m[1])
      m = PLACEHOLDER_RE.exec(cleaned)
    }
    return out
  }

  function insertAt(el, text, value, setValue) {
    const start = el && typeof el.selectionStart === 'number' ? el.selectionStart : value.length
    const end = el && typeof el.selectionEnd === 'number' ? el.selectionEnd : value.length
    const next = value.slice(0, start) + text + value.slice(end)
    setValue(next)
    if (el) {
      later(function () {
        try {
          el.focus()
          el.setSelectionRange(start + text.length, start + text.length)
        } catch (e) { /* the element may be gone */ }
      }, 0)
    }
  }

  function PlaceholderButtons(props) {
    return h('div', { role: 'group', 'aria-label': 'Insert placeholder', style: S.row },
      h('span', { style: S.small }, 'Insert placeholder:'),
      props.names.map(function (n) {
        return h(Btn, { key: n, size: 'xs', variant: 'secondary', disabled: props.disabled, 'aria-label': 'Insert placeholder {' + n + '}',
          style: { fontFamily: V.mono }, onClick: function () { props.onInsert('{' + n + '}') } }, '{' + n + '}')
      }))
  }

  function UploadButton(props) {
    const ref = useRef(null)
    return h('span', null,
      h('input', { ref: ref, type: 'file', accept: '.md,.txt,text/markdown,text/plain', hidden: true, 'aria-hidden': 'true',
        tabIndex: -1, onChange: function (e) {
          const file = e.target.files && e.target.files[0]
          e.target.value = ''
          if (file) readTextFile(file).then(props.onLoaded)
        } }),
      h(Btn, { disabled: props.disabled, onClick: function () { if (ref.current) ref.current.click() } }, 'Upload .md/.txt'))
  }

  function BudgetBar(props) {
    const p = props.preview
    if (!p) return null
    if (p.budget == null) return h('p', { style: S.small }, 'No context budget is set, so the guidance is never shortened for room.')
    const used = Math.max(0, Number(p.guidance_chars_used) || 0)
    const length = Math.max(0, Number(p.length) || 0)
    const fixed = Math.max(0, length - used)
    const free = Math.max(0, Number(p.budget) - length)
    const total = Math.max(Number(p.budget), length, 1)
    const pct = function (n) { return (100 * n / total).toFixed(2) + '%' }
    const legend = 'Left to right: frame and lanes ' + fmtN(fixed) + ' · guidance ' + fmtN(used) + ' · free ' + fmtN(free) +
      ' (budget ' + fmtN(p.budget) + ' characters)'
    return h('div', { style: { display: 'grid', gap: '4px' } },
      h('div', { role: 'img', 'aria-label': legend, style: { display: 'flex', height: '10px', borderRadius: '3px', overflow: 'hidden',
        border: '1px solid ' + V.stroke } },
        h('span', { style: { width: pct(fixed), background: V.text3 } }),
        h('span', { style: { width: pct(used), background: V.accent } }),
        h('span', { style: { width: pct(free), background: V.fill } })),
      h('p', { style: S.small }, legend),
      p.over_budget ? h('p', { style: S.warning }, '⚠ The frame and lanes alone exceed the context budget; the guidance is left out.') : null)
  }

  const GUIDANCE_STATE = {
    included: 'included', shortened: 'shortened to fit', empty: 'empty',
    excluded_operator_lane: 'left out (operator turns, switched off for this server)',
    no_room: 'left out (no room left in the context budget)'
  }

  function PreviewView(props) {
    const p = props.preview
    if (!p) return h('p', { style: S.small }, props.pending ? 'Assembling the preview…' : 'No preview yet.')
    const truncated = (p.truncated_by || []).map(function (t) {
      return t === 'max_chars' ? 'the operator’s guidance limit' : t === 'budget' ? 'the context budget' : String(t)
    })
    const placeholders = p.placeholders && typeof p.placeholders === 'object' ? Object.keys(p.placeholders) : []
    return h('div', { style: { display: 'grid', gap: '8px' } },
      h('p', { style: S.p }, 'Guidance: ' + (GUIDANCE_STATE[p.guidance] || p.guidance || 'unknown') +
        (truncated.length ? ' · shortened by ' + truncated.join(' and ') : '') +
        ' · ' + fmtN(p.length) + ' characters in total'),
      (p.sections || []).map(function (s, i) {
        return h('section', { key: i, 'aria-label': SECTION_TITLES[s.kind] || s.title || s.kind,
          style: { borderLeft: '3px solid ' + (s.kind === 'guidance' ? V.accent : V.stroke), paddingLeft: '10px' } },
        h('h5', { style: S.h4 }, (i + 1) + '. ' + (SECTION_TITLES[s.kind] || s.title || s.kind)),
        h('pre', { style: S.pre }, s.text || '(empty)'))
      }),
      placeholders.length ? h('details', null, h('summary', { style: S.summary }, 'Placeholder values'),
        h('table', { style: S.table }, h('tbody', null, placeholders.map(function (k) {
          return h('tr', { key: k }, h('th', { scope: 'row', style: Object.assign({}, S.td, { fontFamily: V.mono }) }, '{' + k + '}'),
            h('td', { style: S.td }, String(p.placeholders[k])))
        })))) : null,
      (p.warnings || []).map(function (w, i) { return h('p', { key: 'w' + i, style: S.warning }, '⚠ ' + w) }))
  }

  function GuidanceEditor(props) {
    const base = props.base
    const saved = String(getPath(base.settings, 'prompts.guild') || '')
    const [draft, setDraft] = useState(saved)
    const [lane, setLane] = useState('public:ambient')
    const [preview, setPreview] = useState(null)
    const [pending, setPending] = useState(true)
    const [previewError, setPreviewError] = useState(null)
    const [error, setError] = useState(null)
    const [status, setStatus] = useState(null)
    const [busy, setBusy] = useState(false)
    const [discard, setDiscard] = useState(false)
    const [restore, setRestore] = useState(null)
    const [restoreUnavailable, setRestoreUnavailable] = useState(false)
    const [upload, setUpload] = useState(null)
    const taRef = useRef(null)
    const statusRef = useRef(null)
    const seq = useRef(0)
    const audit = useLoad('/v1/audit?bot=' + enc(props.bot) + '&guild=' + enc(props.guild) + '&limit=50', { poll: false })
    const known = (props.schema && props.schema.placeholders) || DEFAULT_PLACEHOLDERS
    const limits = (props.schema && props.schema.limits) || {}
    const max = (preview && preview.check && preview.check.max_chars) || (base.guidance && base.guidance.max_chars) ||
      limits.guidance_max || limits.guidance_hard
    const dirty = draft !== saved
    const taId = 'dsp-guidance-text'
    const errId = taId + '-error'
    const countId = taId + '-count'

    useEffect(function () {
      seq.current += 1
      const n = seq.current
      setPending(true)
      const parts = lane.split(':')
      const cancel = later(function () {
        call('/v1/preview', { method: 'POST', body: { bot: props.bot, guild: props.guild, lane: parts[0], mode: parts[1], draft: draft } })
          .then(function (r) {
            if (n !== seq.current) return
            setPending(false)
            if (r.ok) {
              setPreview(r.data)
              setPreviewError(null)
              if (props.onCheck && r.data && r.data.check) props.onCheck(r.data.check)
            } else {
              setPreviewError(r)
            }
          })
      }, PREVIEW_DEBOUNCE_MS)
      return cancel
    }, [draft, lane])

    useEffect(function () {
      if (props.panel === 'preview' || props.panel === 'history') {
        const el = typeof document !== 'undefined' ? document.getElementById('dsp-guidance-' + props.panel) : null
        if (el && typeof el.scrollIntoView === 'function') el.scrollIntoView({ block: 'start' })
      } else if (taRef.current) {
        taRef.current.focus()
      }
    }, [])

    const check = preview && preview.check
    const fresh = !pending && check
    let counter = fmtN(draft.length) + ' / ' + fmtN(max) + ' characters'
    if (fresh) {
      counter = fmtN(check.chars) + ' / ' + fmtN(check.max_chars) + ' characters · in the context ' + fmtN(check.needed) +
        ' · room left ' + (check.room == null ? 'unlimited' : fmtN(check.room))
    } else {
      counter += ' (provisional, checking…)'
    }
    const unknown = fresh ? (check.unknown_placeholders || preview.unknown_placeholders || []) : unknownPlaceholders(draft, known)
    const problems = fresh && check.problems ? check.problems : []

    const historyItems = []
    const seen = {}
    ;(((audit.data && audit.data.guidance) || []).concat((base.guidance && base.guidance.history) || [])).forEach(function (v) {
      if (!v || !v.sha256 || seen[v.sha256]) return
      seen[v.sha256] = true
      historyItems.push(v)
    })
    historyItems.sort(function (a, b) { return (toMs(b.at) || 0) - (toMs(a.at) || 0) })
    const nameOf = useUserNames(props.bot, historyItems.map(function (v) { return v.by }))

    function save() {
      setBusy(true)
      setError(null)
      return call(gpath(props.bot, props.guild) + '/guidance', { method: 'POST', body: { expected_rev: base.rev, text: draft } })
        .then(function (r) {
          setBusy(false)
          if (r.ok) {
            const next = r.data && r.data.guild
            props.onSaved(next, '✓ Guidance saved' + (next ? ' (rev ' + next.rev + ')' : '') + '.' +
              (r.data && r.data.history === false ? ' This version keeps no history of earlier texts.' : ''))
            return
          }
          if (r.status === 409) {
            props.onConflict(r.conflict || {})
            setStatus('✕ Not saved: this server’s settings changed elsewhere; see the notice at the top. Your text stays here.')
          } else if (r.status === 422) {
            setError(r.message)
            setStatus('✕ Not saved: 1 problem, listed below.')
          } else {
            setStatus('✕ Not saved: ' + errorText(r))
          }
          if (statusRef.current) statusRef.current.focus()
        })
    }

    function doRestore(v) {
      setBusy(true)
      return call(gpath(props.bot, props.guild) + '/guidance/restore', { method: 'POST', body: { expected_rev: base.rev, sha256: v.sha256 } })
        .then(function (r) {
          setBusy(false)
          setRestore(null)
          if (r.ok) {
            props.onSaved(r.data && r.data.guild, '✓ Restored the version of ' + fmtUTC(v.at) + '.')
            return
          }
          if (r.status === 501) setRestoreUnavailable(true)
          if (r.status === 409) props.onConflict(r.conflict || {})
          setStatus(r.status === 501 ? '✕ Restoring is not available in this version.' : '✕ Not restored: ' + errorText(r))
          if (statusRef.current) statusRef.current.focus()
        })
    }

    const ro = props.readOnly
    return h('section', { 'aria-labelledby': 'dsp-guidance-title', style: S.card },
      h('h3', { id: 'dsp-guidance-title', style: S.h3 }, 'Server guidance — ' + props.name),
      h('p', { style: S.small }, 'Guidance tells the bot about this server: tone, topics, rules. It is placed after the ' +
        'operator frame and never overrides it. Saved only with Save.'),
      h('label', { htmlFor: taId, style: { fontWeight: 500 } }, 'Guidance text'),
      h(TextArea, { id: taId, ref: taRef, value: draft, disabled: ro, rows: 14,
        'aria-describedby': joinIds(error ? errId : null, countId), 'aria-invalid': error ? 'true' : undefined,
        onChange: function (e) { setDraft(e.target.value); setError(null) } }),
      error ? h('p', { id: errId, style: S.error }, '✕ ' + error) : null,
      h('p', { id: countId, role: 'status', style: S.small }, counter),
      h(BudgetBar, { preview: fresh ? preview : null }),
      problems.map(function (p, i) { return h('p', { key: 'p' + i, style: S.warning }, '⚠ ' + p) }),
      h(PlaceholderButtons, { names: known, disabled: ro, onInsert: function (t) { insertAt(taRef.current, t, draft, setDraft) } }),
      unknown.map(function (u) {
        return h('p', { key: 'u-' + u, style: S.warning }, '⚠ Unknown placeholder {' + u + '} — it stays as written.')
      }),
      upload ? h('p', { role: 'status', style: upload.error ? S.error : S.small }, upload.error ? '✕ ' + upload.error : upload.text) : null,
      error ? h(ErrorSummary, { items: [{ path: 'prompts.guild', text: error }], sections: [{ fields: [{ path: 'prompts.guild', label: 'Guidance text' }] }] }) : null,
      h('div', { style: Object.assign({}, S.row, { justifyContent: 'space-between' }) },
        h('div', { style: S.row },
          h(UploadButton, { disabled: ro, onLoaded: function (res) {
            if (res.error) setUpload({ error: res.error })
            else {
              setDraft(res.text)
              setUpload({ text: 'Loaded ' + fmtN(res.text.length) + ' characters from the file. Nothing is saved until you press Save.' })
            }
          } }),
          h(Btn, { onClick: function () { download('guidance-' + props.guild + '.md', draft, 'text/markdown') } }, 'Download')),
        h('div', { style: S.rowEnd },
          discard ? h('span', { role: 'group', 'aria-label': 'Discard changes', style: S.row },
            h('span', null, 'Discard the unsaved guidance?'),
            h(Btn, { variant: 'destructive', onClick: props.onClose }, 'Discard'),
            h(Btn, { variant: 'text', onClick: function () { setDiscard(false) } }, 'Keep editing'))
            : h(Btn, { variant: 'text', onClick: function () { if (dirty) setDiscard(true); else props.onClose() } }, 'Cancel'),
          ro ? null : h(Btn, { variant: 'default', disabled: busy || !dirty, onClick: save }, busy ? 'Saving…' : 'Save'))),
      h('p', { ref: statusRef, role: 'status', tabIndex: -1, style: Object.assign({}, S.small, { outline: 'none' }) }, status || ''),
      h('section', { id: 'dsp-guidance-preview', 'aria-labelledby': 'dsp-guidance-preview-title', style: { display: 'grid', gap: '8px' } },
        h('div', { style: Object.assign({}, S.row, { justifyContent: 'space-between' }) },
          h('h4', { id: 'dsp-guidance-preview-title', style: Object.assign({}, S.h3) }, 'Preview of the assembled context'),
          h(Segmented, { label: 'Lane and mode', value: lane, options: PREVIEW_CHOICES, onChange: setLane })),
        previewError ? h('p', { style: S.warning }, '⚠ Preview not available: ' + errorText(previewError)) : null,
        h(PreviewView, { preview: fresh ? preview : null, pending: pending })),
      h('section', { id: 'dsp-guidance-history', 'aria-labelledby': 'dsp-guidance-history-title', style: { display: 'grid', gap: '6px' } },
        h('h4', { id: 'dsp-guidance-history-title', style: S.h3 }, 'History (' + historyItems.length + ')'),
        props.features && props.features.guidance_ops === false
          ? h('p', { style: S.small }, 'ⓘ This version keeps no history of earlier guidance texts.') : null,
        historyItems.length ? h('ul', { style: S.list }, historyItems.map(function (v) {
          return h('li', { key: v.sha256, style: { padding: '4px 0', borderBottom: '1px solid ' + V.stroke } },
            h('div', { style: S.row },
              h('span', null, fmtUTC(v.at)),
              h('span', { style: S.secondary }, whoText(v, nameOf) + ' · ' + plural(v.chars || 0, 'character', 'characters') + ' · ' +
                String(v.sha256).slice(0, 8)),
              ro || restoreUnavailable ? null : (restore === v.sha256
                ? h('span', { role: 'group', 'aria-label': 'Confirm restore', style: S.row },
                  h('span', null, 'Restore this version? It replaces the current guidance at once.'),
                  h(Btn, { variant: 'default', disabled: busy, onClick: function () { doRestore(v) } }, 'Restore'),
                  h(Btn, { variant: 'text', onClick: function () { setRestore(null) } }, 'Cancel'))
                : h(Btn, { variant: 'text', onClick: function () { setRestore(v.sha256) } }, 'Restore…'))),
            v.diff ? h('details', null, h('summary', { style: S.small }, 'Changes against the current text'),
              h('pre', { style: S.pre }, v.diff)) : null)
        })) : h('p', { style: S.small }, 'No earlier versions.'),
        restoreUnavailable ? h('p', { style: S.small }, 'Restoring is not available in this version.') : null))
  }

  // ── Servers page ──────────────────────────────────────────────────────────────────────────────────

  function Servers() {
    const meta = useLoad('/v1/meta')
    const ov = useLoad('/v1/overview')
    const [sel, setSel] = useState(readSelection)
    useEffect(function () {
      const fn = function (s) { setSel(s) }
      selectionListeners.add(fn)
      return function () { selectionListeners.delete(fn) }
    }, [])
    const data = ov.data
    const bots = (data && data.bots) || []
    const many = bots.length > 1
    const options = []
    bots.forEach(function (b) {
      ;(b.guilds || []).forEach(function (g) {
        options.push({ key: b.id + '/' + g.id, label: g.name || 'Server ' + g.id, terms: String(g.id),
          hint: stateLabel(g).slice(2) + (many ? ' · ' + (b.name || b.id) : ''), group: many ? (b.name || 'Bot ' + shortId(b.id)) : undefined,
          bot: String(b.id), guild: String(g.id) })
      })
    })
    const broken = storeBroken(data)
    const readOnly = broken || (meta.data && meta.data.mode === 'read-only')
    const selKey = sel ? sel.bot + '/' + sel.guild : null
    const selBot = sel ? bots.filter(function (b) { return String(b.id) === sel.bot })[0] : null
    const header = { title: 'Servers', meta: meta.data, readOnly: readOnly, directoryAt: newestDirectory(data) }
    if (!data && ov.error) return h(Page, header, h(LoadError, { error: ov.error, onRetry: ov.reload }))
    if (!data) return h(Page, header, h(Loading))
    const parts = []
    if (broken) parts.push(h(StoreProblem, { key: 'store', store: data.store }))
    if (!options.length && !sel) {
      parts.push(h('div', { key: 'empty', style: S.card },
        h(K.EmptyState, { title: 'No servers yet', description: bots.length
          ? 'The bot is in no server yet. Invite it to a server; it appears here once Discord reports it.'
          : 'Servers appear once the Hermes gateway has connected the bot to Discord.' }),
        h('div', { style: { display: 'flex', justifyContent: 'center' } },
          h(Btn, { onClick: function () { nav.go('overview') } }, 'Back to the overview'))))
      return h(Page, header, parts)
    }
    parts.push(h('div', { key: 'pick', style: { display: 'grid', gap: '4px' } },
      h('span', { id: 'dsp-server-pick-label', style: { fontWeight: 500 } }, 'Server'),
      h(Combobox, { id: 'dsp-server-pick', labelledBy: 'dsp-server-pick-label', single: true, options: options,
        noun: ['server', 'servers'], placeholder: 'search servers…',
        selected: sel ? [{ key: selKey, label: labelFrom(options, selKey, 'Server ' + sel.guild) }] : [],
        onAdd: function (o) { writeSelection({ bot: o.bot, guild: o.guild }) },
        onRemove: function () { writeSelection(null) } })))
    if (!sel) {
      parts.push(h('div', { key: 'none', style: S.card }, h(K.EmptyState, { title: 'Pick a server',
        description: 'Search above, or open a server from the overview.' })))
    } else {
      parts.push(h(ServerDetail, { key: selKey, bot: sel.bot, guild: sel.guild, meta: meta.data, readOnly: readOnly,
        online: selBot ? selBot.online : undefined }))
    }
    return h(Page, header, parts)
  }

  // ── Operators page ────────────────────────────────────────────────────────────────────────────────

  const SETTING_TEXT = {
    operators: { label: 'Operators', help: 'Discord users with tool authority. Only settable here or on the host ' +
      '(hermes config set …), never from Discord.' },
    operator_label: { label: 'Operator label', help: 'How public texts refer to the operator ({operator_label}).' },
    guild_prompt_max_chars: { label: 'Guidance limit', help: 'Upper bound for every server’s guidance, in characters.' },
    notify_operators: { label: 'Direct-message operators about requests, guidance changes, protection',
      help: 'Operators get a direct message from the bot when a server asks for activation, guidance changes, or ' +
        'protection degrades.' },
    frame_prompt: { label: 'Operator frame', help: 'Placed before every server’s guidance; servers cannot change it. ' +
      'Empty = built-in neutral frame.' }
  }

  function useMemberSearch(bot, guild, query, enabled) {
    const [state, setState] = useState({ results: [], busy: false, unavailable: null })
    const seq = useRef(0)
    useEffect(function () {
      seq.current += 1
      const n = seq.current
      const q = query.trim()
      if (!enabled || !bot || !guild || q.length < 2 || ID_RE.test(q)) {
        setState(function (prev) { return { results: [], busy: false, unavailable: prev.unavailable } })
        return undefined
      }
      setState(function (prev) { return { results: prev.results, busy: true, unavailable: prev.unavailable } })
      return later(function () {
        call('/v1/members/' + enc(bot) + '/' + enc(guild) + '?q=' + enc(q)).then(function (r) {
          if (n !== seq.current) return
          if (r.ok) setState({ results: (r.data && r.data.results) || [], busy: false, unavailable: null, note: r.data && r.data.note })
          else setState({ results: [], busy: false, unavailable: r.status === 503 ? (r.message || 'unavailable') : null,
            failed: r.status === 503 ? null : errorText(r) })
        })
      }, MEMBER_DEBOUNCE_MS)
    }, [bot, guild, query, enabled])
    return state
  }

  function Operators() {
    const meta = useLoad('/v1/meta')
    const ov = useLoad('/v1/overview')
    const st = useLoad('/v1/settings')
    const schemaL = useLoad('/v1/schema', { poll: false })
    const [loaded, setLoaded] = useState(null)
    const [draft, setDraft] = useState(null)
    const [conflict, setConflict] = useState(false)
    const [errors, setErrors] = useState({})
    const [summary, setSummary] = useState([])
    const [status, setStatus] = useState(null)
    const [busy, setBusy] = useState(false)
    const [locked, setLocked] = useState(null)
    const [frameOpen, setFrameOpen] = useState(false)
    const [query, setQuery] = useState('')
    const [searchIn, setSearchIn] = useState(function () { return sget('searchIn', null) })
    const statusRef = useRef(null)
    const frameRef = useRef(null)
    const [focusStatus, setFocusStatus] = useState(0)

    const dirty = !!(loaded && draft && !equal(draft, loaded.values))
    useEffect(function () {
      if (!st.data) return
      if (!loaded || (!dirty && !conflict)) {
        setLoaded(st.data)
        setDraft(clone(st.data.values || {}))
      }
    }, [st.data])
    useEffect(function () { if (focusStatus && statusRef.current) statusRef.current.focus() }, [focusStatus])

    const data = ov.data
    const bots = (data && data.bots) || []
    const guildOptions = []
    bots.forEach(function (b) {
      ;(b.guilds || []).forEach(function (g) {
        guildOptions.push({ key: b.id + '/' + g.id, label: g.name || 'Server ' + g.id, bot: String(b.id), guild: String(g.id),
          group: bots.length > 1 ? (b.name || 'Bot ' + shortId(b.id)) : undefined })
      })
    })
    const pick = guildOptions.filter(function (o) { return o.key === searchIn })[0] || guildOptions[0] || null
    const features = (meta.data && meta.data.features) || {}
    const searchOff = features.member_search === false
    const search = useMemberSearch(pick && pick.bot, pick && pick.guild, query, !searchOff && !!pick)
    const nameBot = pick ? pick.bot : (bots[0] ? String(bots[0].id) : null)
    const ops = draft && Array.isArray(draft.operators) ? draft.operators.map(String) : []
    const savedOps = loaded && loaded.values && Array.isArray(loaded.values.operators) ? loaded.values.operators.map(String) : []
    const nameOf = useUserNames(nameBot, ops.concat(savedOps))
    const hard = (schemaL.data && schemaL.data.limits && schemaL.data.limits.guidance_hard) || 20000
    const readable = !loaded || loaded.readable !== false
    const ro = !readable || !!locked
    const keyInfo = {}
    ;((loaded && loaded.keys) || []).forEach(function (k) { keyInfo[k.key] = k })
    const textOf = function (key, what) {
      return (keyInfo[key] && keyInfo[key][what]) || (SETTING_TEXT[key] && SETTING_TEXT[key][what]) || (what === 'label' ? key : '')
    }

    const header = { title: 'Operators and operator frame', meta: meta.data }
    if (!loaded && st.error) return h(Page, header, h(LoadError, { error: st.error, onRetry: st.reload }))
    if (!loaded || !draft) return h(Page, header, h(Loading))

    function setKey(key, value) {
      setDraft(function (prev) { const next = Object.assign({}, prev); next[key] = value; return next })
      setErrors(function (prev) { return omit(prev, [key]) })
    }

    function opLabel(id) {
      const name = nameOf(id)
      return name ? name + ' (' + shortId(id) + ')' : id
    }

    function save() {
      const values = {}
      Object.keys(draft).forEach(function (k) { if (!equal(draft[k], loaded.values[k])) values[k] = draft[k] })
      const local = []
      if (hasOwn(values, 'guild_prompt_max_chars')) {
        const v = values.guild_prompt_max_chars
        if (typeof v !== 'number' || !Number.isInteger(v) || v < 0 || v > hard) {
          local.push({ key: 'guild_prompt_max_chars', text: 'Enter a whole number between 0 and ' + fmtN(hard) + '.' })
        }
      }
      if (local.length) {
        const errs = {}
        local.forEach(function (p) { errs[p.key] = p.text })
        setErrors(errs)
        setSummary(local)
        setStatus('✕ Not saved: ' + plural(local.length, 'problem', 'problems') + ', listed below.')
        setFocusStatus(function (n) { return n + 1 })
        return Promise.resolve()
      }
      setBusy(true)
      return call('/v1/settings', { method: 'PUT', body: { expected_digest: loaded.digest, values: values } }).then(function (r) {
        setBusy(false)
        if (r.ok) {
          const next = Object.assign({}, loaded, { values: r.data.values || loaded.values, digest: r.data.digest })
          setLoaded(next)
          setDraft(clone(next.values))
          setErrors({})
          setSummary([])
          const written = (r.data.written || Object.keys(values)).map(function (k) { return textOf(k, 'label') })
          setStatus('✓ Saved' + (written.length ? ': ' + written.join(', ') : '') + '. Takes effect on the next turn.')
        } else if (r.status === 409) {
          setConflict(true)
          setStatus('✕ Not saved: the operator settings changed elsewhere; see the notice at the top.')
        } else if (r.status === 422) {
          const key = r.field ? String(r.field).split(/[.[]/)[0] : null
          const errs = {}
          if (key) errs[key] = r.message
          setErrors(errs)
          setSummary([{ key: key, text: r.message }])
          setStatus('✕ Not saved: 1 problem, listed below.')
        } else if (r.status === 423) {
          setLocked(r.message || 'read-only')
          setStatus('✕ Not saved: ' + errorText(r))
        } else {
          setStatus('✕ Not saved: ' + errorText(r))
        }
        setFocusStatus(function (n) { return n + 1 })
      })
    }

    function reload() {
      return st.reload().then(function (r) {
        if (r && r.ok) {
          setLoaded(r.data)
          setDraft(clone(r.data.values || {}))
          setConflict(false)
          setErrors({})
          setSummary([])
          setStatus(null)
          setLocked(null)
        }
      })
    }

    const removed = savedOps.filter(function (id) { return ops.indexOf(id) < 0 })
    const opsErr = errors.operators
    const parts = []
    if (conflict) {
      parts.push(h('div', { key: 'conflict', role: 'alert', style: S.banner },
        h('span', null, '⚠ Not saved: the operator settings were changed elsewhere after you opened this page. '),
        h(Btn, { onClick: reload }, 'Reload'),
        h('span', null, ' — your unsaved edits stay in the form until you reload.')))
    }
    if (!readable) {
      parts.push(h(Banner, { key: 'unreadable', role: 'alert' }, '⚠ The Hermes configuration could not be read' +
        (loaded.error ? ' (' + loaded.error + ')' : '') + '. Nobody counts as an operator until it is fixed, and this page ' +
        'cannot save meanwhile.'))
    }
    if (locked) parts.push(h(Banner, { key: 'locked', role: 'alert' }, '⚠ Read-only: ' + locked))

    const memberOptions = (search.results || []).map(function (m) {
      return { key: String(m.id), label: m.display_name || m.name || String(m.id),
        hint: (m.name ? '@' + m.name + ' · ' : '') + m.id }
    })
    let live = null
    if (searchOff || search.unavailable) live = 'Member search is not available here; paste a user ID.'
    else if (!pick) live = 'No server to search in; paste a user ID.'
    else if (query.trim().length < 2) live = 'Type 2 or more characters to search members of ' + pick.label + ', or paste a user ID.'
    else if (search.busy) live = 'Searching…'
    else if (search.failed) live = 'Search failed: ' + search.failed
    const opsFid = 'dsp-op-operators'
    parts.push(h('section', { key: 'ops', 'aria-labelledby': 'dsp-op-title', style: S.card },
      h('h3', { id: 'dsp-op-title', style: S.h3 }, 'Operators'),
      h('p', { style: S.small }, textOf('operators', 'help')),
      h('span', { id: opsFid + '-label', style: { fontWeight: 500 } }, 'Operator list'),
      h(Combobox, {
        id: opsFid, labelledBy: opsFid + '-label', selected: ops.map(function (id) { return { key: id, label: opLabel(id) } }),
        options: memberOptions, filter: false, noun: ['member', 'members'], liveText: live,
        placeholder: searchOff || search.unavailable || !pick ? 'paste a user ID…' : 'search a server’s members or paste a user ID…',
        describedBy: opsErr ? opsFid + '-error' : undefined, invalid: !!opsErr, disabled: ro,
        onQueryChange: setQuery,
        freeOptions: function (text, chosen) { return idOption(text, chosen, null) },
        onAdd: function (o) { setKey('operators', ops.concat([String(o.key)])) },
        onRemove: function (k) { setKey('operators', ops.filter(function (x) { return x !== String(k) })) }
      }),
      opsErr ? h('p', { id: opsFid + '-error', style: S.error }, '✕ ' + opsErr) : null,
      guildOptions.length && !searchOff ? h('div', { style: { display: 'grid', gap: '4px', maxWidth: '28em' } },
        h('span', { id: 'dsp-op-searchin-label', style: S.small }, 'search in:'),
        h(Combobox, { id: 'dsp-op-searchin', labelledBy: 'dsp-op-searchin-label', single: true, options: guildOptions,
          noun: ['server', 'servers'], placeholder: 'pick a server…',
          selected: pick ? [{ key: pick.key, label: pick.label }] : [],
          onAdd: function (o) { setSearchIn(o.key); sset('searchIn', o.key) },
          onRemove: function () { setSearchIn(null); sremove('searchIn') } })) : null,
      search.unavailable ? h('p', { style: S.small }, 'ⓘ Member search is not available (' + search.unavailable +
        '). Paste a user ID instead; the name appears once the bot can resolve it.') : null,
      ops.length === 0 ? h('p', { style: S.warning }, 'ⓘ No operator: only public lanes; activation only from this page or the CLI.') : null,
      removed.length ? h('p', { role: 'status', style: S.warning }, '⚠ You are removing ' + removed.map(opLabel).join(', ') +
        '. Removing yourself: you keep host access, but lose tool authority in Discord on the next turn.') : null))

    const labelErr = errors.operator_label
    const maxErr = errors.guild_prompt_max_chars
    const frame = String(draft.frame_prompt || '')
    const extraKeys = Object.keys(draft).filter(function (k) {
      return ['operators', 'operator_label', 'guild_prompt_max_chars', 'notify_operators', 'frame_prompt'].indexOf(k) < 0
    })
    parts.push(h('section', { key: 'settings', 'aria-label': 'Operator settings', style: S.card },
      h(K.ListRow, {
        title: h('span', { id: 'dsp-op-label-label' }, textOf('operator_label', 'label')),
        description: textOf('operator_label', 'help'),
        action: h(TextInput, { id: 'dsp-op-label', value: String(draft.operator_label || ''), disabled: ro,
          'aria-labelledby': 'dsp-op-label-label', 'aria-describedby': labelErr ? 'dsp-op-label-error' : undefined,
          'aria-invalid': labelErr ? 'true' : undefined, style: { minWidth: '14em' },
          onChange: function (e) { setKey('operator_label', e.target.value) } }),
        below: labelErr ? h('p', { id: 'dsp-op-label-error', style: S.error }, '✕ ' + labelErr) : null
      }),
      h(K.ListRow, {
        title: h('span', { id: 'dsp-op-max-label' }, textOf('guild_prompt_max_chars', 'label')),
        description: textOf('guild_prompt_max_chars', 'help'),
        action: h('div', { style: Object.assign({}, S.row, { flexWrap: 'nowrap' }) },
          h(TextInput, { id: 'dsp-op-max', type: 'number', inputMode: 'numeric', min: 0, max: hard, step: 1, disabled: ro,
            value: draft.guild_prompt_max_chars == null ? '' : String(draft.guild_prompt_max_chars),
            'aria-labelledby': 'dsp-op-max-label', 'aria-describedby': joinIds(maxErr ? 'dsp-op-max-error' : null, 'dsp-op-max-hint'),
            'aria-invalid': maxErr ? 'true' : undefined, style: { width: '7em' },
            onChange: function (e) { const raw = e.target.value; setKey('guild_prompt_max_chars', /^[0-9]+$/.test(raw) ? Number(raw) : raw) } }),
          h('span', { id: 'dsp-op-max-hint', style: Object.assign({}, S.small, { whiteSpace: 'nowrap' }) },
            'characters (hard ceiling ' + fmtN(hard) + ')')),
        below: maxErr ? h('p', { id: 'dsp-op-max-error', style: S.error }, '✕ ' + maxErr) : null
      }),
      h(K.ToggleRow, { label: textOf('notify_operators', 'label'), description: textOf('notify_operators', 'help'),
        checked: !!draft.notify_operators, disabled: ro, onChange: function (on) { setKey('notify_operators', !!on) },
        below: errors.notify_operators ? h('p', { style: S.error }, '✕ ' + errors.notify_operators) : null }),
      extraKeys.map(function (k) {
        const info = keyInfo[k] || {}
        const v = draft[k]
        if (typeof v === 'boolean' || info.kind === 'bool') {
          return h(K.ToggleRow, { key: k, label: textOf(k, 'label'), description: textOf(k, 'help'), checked: !!v, disabled: ro,
            onChange: function (on) { setKey(k, !!on) } })
        }
        return h(K.ListRow, { key: k, title: h('span', { id: 'dsp-op-' + k + '-label' }, textOf(k, 'label')), description: textOf(k, 'help'),
          action: h(TextInput, { value: v == null ? '' : (typeof v === 'object' ? JSON.stringify(v) : String(v)), disabled: ro || typeof v === 'object',
            'aria-labelledby': 'dsp-op-' + k + '-label',
            onChange: function (e) { setKey(k, typeof v === 'number' && /^-?[0-9]+$/.test(e.target.value) ? Number(e.target.value) : e.target.value) } }),
          below: errors[k] ? h('p', { style: S.error }, '✕ ' + errors[k]) : null })
      }),
      h(K.ListRow, {
        title: textOf('frame_prompt', 'label'),
        description: textOf('frame_prompt', 'help') + ' ' + (frame ? fmtN(frame.length) + ' characters.' : 'Currently empty.'),
        action: h(Btn, { 'aria-expanded': frameOpen ? 'true' : 'false', 'aria-controls': 'dsp-op-frame-editor',
          onClick: function () { setFrameOpen(!frameOpen) } }, frameOpen ? 'Close the frame editor' : 'Edit frame'),
        wide: true,
        below: frameOpen ? h('div', { id: 'dsp-op-frame-editor', style: { display: 'grid', gap: '6px', marginTop: '6px' } },
          h('label', { htmlFor: 'dsp-op-frame' }, 'Operator frame text'),
          h(TextArea, { id: 'dsp-op-frame', ref: frameRef, value: frame, disabled: ro, rows: 10,
            'aria-describedby': errors.frame_prompt ? 'dsp-op-frame-error' : 'dsp-op-frame-count',
            onChange: function (e) { setKey('frame_prompt', e.target.value) } }),
          errors.frame_prompt ? h('p', { id: 'dsp-op-frame-error', style: S.error }, '✕ ' + errors.frame_prompt) : null,
          h('p', { id: 'dsp-op-frame-count', style: S.small }, fmtN(frame.length) + ' characters' + (frame ? '' : ' (built-in neutral frame)')),
          h(PlaceholderButtons, { names: (schemaL.data && schemaL.data.placeholders) || DEFAULT_PLACEHOLDERS, disabled: ro,
            onInsert: function (t) { insertAt(frameRef.current, t, frame, function (v) { setKey('frame_prompt', v) }) } }),
          unknownPlaceholders(frame, (schemaL.data && schemaL.data.placeholders) || DEFAULT_PLACEHOLDERS).map(function (u) {
            return h('p', { key: u, style: S.warning }, '⚠ Unknown placeholder {' + u + '} — it stays as written.')
          }),
          h('div', { style: S.row }, h(UploadButton, { disabled: ro, onLoaded: function (res) {
            if (res.error) setStatus('✕ ' + res.error)
            else setKey('frame_prompt', res.text)
          } }))) : null
      })))

    if (summary.length) {
      parts.push(h('div', { key: 'summary', role: 'alert', style: Object.assign({}, S.banner, { display: 'grid' }) },
        h('p', { style: S.error }, '✕ Not saved — ' + plural(summary.length, 'problem', 'problems') + ':'),
        h('ul', { style: S.list }, summary.map(function (s, i) {
          return h('li', { key: i, style: S.small }, (s.key ? textOf(s.key, 'label') : 'Request') + ': ' + s.text)
        }))))
    }
    parts.push(h('div', { key: 'save', style: Object.assign({}, S.row, { justifyContent: 'space-between' }) },
      h('p', { ref: statusRef, role: 'status', tabIndex: -1, style: Object.assign({}, S.small, { outline: 'none' }) },
        status || (dirty ? '● Unsaved changes' : '')),
      ro ? null : h(Btn, { variant: 'default', disabled: !dirty || busy, onClick: save }, busy ? 'Saving…' : 'Save')))
    return h(Page, header, parts)
  }

  // ── Status page ───────────────────────────────────────────────────────────────────────────────────

  function Status() {
    const meta = useLoad('/v1/meta')
    const st = useLoad('/v1/status')
    const hist = useLoad('/v1/status/history')
    const header = { title: 'Protection status', meta: meta.data }
    if (!st.data && st.error) return h(Page, header, h(LoadError, { error: st.error, onRetry: st.reload }))
    if (!st.data) return h(Page, header, h(Loading))
    const d = st.data
    const entries = (hist.data && hist.data.entries) || []
    const parts = []
    const line = []
    if (d.plugin_version) line.push('plugin ' + d.plugin_version)
    if (d.hermes_version) line.push('Hermes ' + d.hermes_version)
    if (d.pid != null) line.push('gateway pid ' + d.pid)
    if (d.at != null) line.push(relTime(d.at))
    parts.push(h('p', { key: 'line', style: S.secondary }, line.join(' · ') || 'No version information.'))
    if (!d.status && !(d.interventions || []).length) {
      parts.push(h('div', { key: 'empty', style: S.card }, h(K.EmptyState, { title: 'No status yet',
        description: 'The gateway writes the protection status when it starts with this plugin enabled. Start or restart ' +
          'the gateway, then come back.' })))
    } else {
      const armed = d.armed_profiles || []
      parts.push(h('p', { key: 'armed', style: S.p }, armed.length ? '✓ Armed in ' + armed.join(', ') + '.'
        : '⚠ Not armed in any profile.'))
      parts.push(h('div', { key: 'table', style: { overflowX: 'auto' } }, h('table', { style: S.table },
        h('caption', { style: Object.assign({}, S.small, { textAlign: 'left', paddingBottom: '4px' }) },
          'Interventions. Class S = security (fails closed), K = comfort (falls back to Hermes’ own behaviour).'),
        h('thead', null, h('tr', null, ['Class', 'Intervention', 'Outcome', 'Reason', 'Since'].map(function (t) {
          return h('th', { key: t, scope: 'col', style: S.th }, t)
        }))),
        h('tbody', null, (d.interventions || []).map(function (iv) {
          return h('tr', { key: iv.id },
            h('td', { style: S.td }, h('abbr', { title: iv.klass === 'S' ? 'security' : 'comfort' }, iv.klass)),
            h('td', { style: Object.assign({}, S.td, { fontFamily: V.mono }) }, iv.id),
            h('td', { style: S.td }, iv.outcome),
            h('td', { style: S.td }, iv.reason || '—'),
            h('td', { style: S.td }, iv.at != null ? fmtUTC(iv.at) : '—'))
        })))))
    }
    if (d.page) {
      parts.push(h('p', { key: 'page', style: S.small }, 'Settings page backend: ' + (d.page.process || 'unknown process') +
        (d.page.at != null ? ' · ' + relTime(d.page.at) : '')))
    }
    parts.push(h('section', { key: 'history', 'aria-labelledby': 'dsp-status-history', style: S.card },
      h('h3', { id: 'dsp-status-history', style: S.h3 }, 'History'),
      entries.length ? h('ul', { style: S.list }, entries.map(function (e, i) {
        return h('li', { key: i, style: S.small }, fmtUTC(e.at) + ' · ' + e.id + ' · ' + (e.from || '—') + ' → ' + (e.to || '—') +
          (e.reason ? ' · ' + e.reason : '') + (e.hermes_version ? ' · Hermes ' + e.hermes_version : ''))
      })) : h('p', { style: S.small }, 'No changes recorded yet.')))
    parts.push(h('div', { key: 'dl' }, h(Btn, { onClick: function () {
      download('status.json', JSON.stringify(d.status != null ? d.status : d, null, 2) + '\n', 'application/json')
    } }, 'Download status.json')))
    return h(Page, header, parts)
  }

  // ── Activity page ─────────────────────────────────────────────────────────────────────────────────

  const ORIGINS = [
    { id: 'all', label: 'All origins' },
    { id: 'discord', label: 'Discord' },
    { id: 'dashboard', label: 'Settings page' },
    { id: 'cli', label: 'Host command line' }
  ]

  function Activity() {
    const meta = useLoad('/v1/meta')
    const ov = useLoad('/v1/overview')
    const [server, setServer] = useState(null)
    const [field, setField] = useState('')
    const [origin, setOrigin] = useState('all')
    const path = '/v1/audit?' + (server ? 'bot=' + enc(server.bot) + '&guild=' + enc(server.guild) + '&' : '') + 'limit=200'
    const audit = useLoad(path)
    const bots = (ov.data && ov.data.bots) || []
    const names = {}
    const options = []
    bots.forEach(function (b) {
      ;(b.guilds || []).forEach(function (g) {
        names[b.id + '/' + g.id] = g.name || 'Server ' + g.id
        options.push({ key: b.id + '/' + g.id, label: g.name || 'Server ' + g.id, bot: String(b.id), guild: String(g.id) })
      })
    })
    const entries = (audit.data && audit.data.entries) || []
    const q = field.trim().toLowerCase()
    const shown = entries.filter(function (e) {
      return (origin === 'all' || e.via === origin) && (!q || String(e.field || '').toLowerCase().indexOf(q) >= 0)
    })
    const firstBot = bots.length ? String(bots[0].id) : null
    const versions = (audit.data && audit.data.guidance) || []
    const nameOf = useUserNames(server ? server.bot : firstBot,
      shown.concat(versions).map(function (e) { return e.by }))
    const header = { title: 'Activity', meta: meta.data }
    const parts = []
    parts.push(h('div', { key: 'filters', role: 'group', 'aria-label': 'Filters', style: Object.assign({}, S.card, { gridTemplateColumns: 'repeat(auto-fit, minmax(14em, 1fr))' }) },
      h('div', { style: { display: 'grid', gap: '4px' } },
        h('span', { id: 'dsp-act-server-label', style: S.small }, 'Server'),
        h(Combobox, { id: 'dsp-act-server', labelledBy: 'dsp-act-server-label', single: true, options: options,
          noun: ['server', 'servers'], placeholder: 'all servers',
          selected: server ? [{ key: server.bot + '/' + server.guild, label: names[server.bot + '/' + server.guild] || server.guild }] : [],
          onAdd: function (o) { setServer({ bot: o.bot, guild: o.guild }) }, onRemove: function () { setServer(null) } })),
      h('div', { style: { display: 'grid', gap: '4px' } },
        h('label', { htmlFor: 'dsp-act-field', style: S.small }, 'Setting'),
        h(TextInput, { id: 'dsp-act-field', type: 'search', placeholder: 'e.g. conversation.ambient_channels', value: field,
          onChange: function (e) { setField(e.target.value) } })),
      h('div', { style: { display: 'grid', gap: '4px' } },
        h('span', { id: 'dsp-act-origin-label', style: S.small }, 'Origin'),
        h(Segmented, { labelledBy: 'dsp-act-origin-label', value: origin, options: ORIGINS, onChange: setOrigin }))))
    if (!audit.data && audit.error) {
      parts.push(h(LoadError, { key: 'err', error: audit.error, onRetry: audit.reload }))
      return h(Page, header, parts)
    }
    if (!audit.data) {
      parts.push(h(Loading, { key: 'loading' }))
      return h(Page, header, parts)
    }
    parts.push(h('p', { key: 'note', style: S.small }, 'Values are stored only as fingerprints, so each entry says that a setting ' +
      'changed, not what it changed to. Server guidance keeps its full text in the history below.'))
    parts.push(shown.length ? h('div', { key: 'table', style: { overflowX: 'auto' } }, h('table', { style: S.table },
      h('caption', { style: Object.assign({}, S.small, { textAlign: 'left', paddingBottom: '4px' }) },
        plural(shown.length, 'change', 'changes') + (entries.length !== shown.length ? ' of ' + fmtN(entries.length) : '') + ', newest first'),
      h('thead', null, h('tr', null, ['When', 'Server', 'Setting', 'Change', 'By', 'Via'].map(function (t) {
        return h('th', { key: t, scope: 'col', style: S.th }, t)
      }))),
      h('tbody', null, shown.map(function (e, i) {
        return h('tr', { key: i },
          h('td', { style: S.td }, fmtUTC(e.at)),
          h('td', { style: S.td }, names[e.bot + '/' + e.guild] || e.guild),
          h('td', { style: Object.assign({}, S.td, { fontFamily: V.mono }) }, e.field),
          h('td', { style: S.td }, 'changed'),
          h('td', { style: S.td }, whoText(e, nameOf)),
          h('td', { style: S.td }, viaText(e.via) || '—'))
      })))) : h('p', { key: 'none', role: 'status', style: S.small }, entries.length ? 'No change matches the filters.' : 'No changes recorded yet.'))
    if (server) {
      parts.push(h('section', { key: 'guidance', 'aria-labelledby': 'dsp-act-guidance', style: S.card },
        h('h3', { id: 'dsp-act-guidance', style: S.h3 }, 'Guidance versions'),
        versions.length ? h('ul', { style: S.list }, versions.map(function (v) {
          return h('li', { key: v.sha256, style: { padding: '4px 0' } },
            h('span', null, fmtUTC(v.at) + ' · ' + whoText(v, nameOf) + ' · ' + plural(v.chars || 0, 'character', 'characters')),
            v.diff ? h('details', null, h('summary', { style: S.small }, 'Changes'), h('pre', { style: S.pre }, v.diff)) : null)
        })) : h('p', { style: S.small }, 'No guidance versions recorded.')))
    }
    return h(Page, header, parts)
  }

  return {
    Overview: Overview,
    Servers: Servers,
    Operators: Operators,
    Status: Status,
    Activity: Activity,
    pages: PAGES.map(function (p) {
      return { id: p.id, title: p.title, component: { servers: Servers, operators: Operators, status: Status, activity: Activity }[p.id] }
    }),
    components: { Combobox: Combobox, ConflictBanner: ConflictBanner, OfflineBanner: OfflineBanner, GuidanceEditor: GuidanceEditor },
    util: { parseRestError: parseRestError, call: call, fmtUTC: fmtUTC, relTime: relTime },
    dispose: function () {
      cache.clear()
      userNames.clear()
      selectionListeners.clear()
    }
  }
}
