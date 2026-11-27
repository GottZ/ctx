// Pure helpers of the recordings view (issue #575): base64 windows to bytes,
// the lossless hex view of bytes that are not valid UTF-8, the default
// translation language, the wall-clock → RFC 3339 filter bound, the named
// error states of a recordings call, the entry links' condition and the size
// formatter.

import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { ApiError } from '../../lib/api'
import { capabilitiesFor } from '../../lib/auth/capabilities'
import type { WhoamiResponse } from '../../lib/api/types'
import { RECORDING_LANGUAGES } from '../../lib/api/recordings'
import {
  decodeBase64,
  hexDump,
  localInputToRfc3339,
  pickDefaultLanguage,
  recordingError,
  fmtBytes,
} from './recording-data'
import { showRecordingsLink } from './recording-link'

function caps(admin: boolean, role = 'owner') {
  const w: WhoamiResponse = {
    success: true,
    label: 'k',
    home_scope: 'private',
    read_scopes: ['private'],
    admin,
    tenant_id: 't',
    role,
    api_key_id: 'k1',
    tenant_slug: 'default',
    tenant_display_name: 'Default',
    capabilities: { model_api: { enabled: true, self_service: true, key_max_lifetime_s: 1, rotation_grace_s: 1, expiry_warn_s: 1 } },
  }
  return capabilitiesFor(w)
}

describe('decodeBase64 / hexDump', () => {
  it('decodes a window losslessly, every byte value included', () => {
    const all = Uint8Array.from({ length: 256 }, (_, i) => i)
    const b64 = Buffer.from(all).toString('base64')
    expect([...decodeBase64(b64)]).toEqual([...all])
    expect(decodeBase64('')).toEqual(new Uint8Array(0))
  })

  it('hex-dumps bytes with their offset, 16 per line, lossless', () => {
    const bytes = Uint8Array.from([0xff, 0xfe, 0x00, 0x41, 0x0a, 0x80, 0x7f, 0x20, 0, 1, 2, 3, 4, 5, 6, 7, 0xe2, 0x82])
    const dump = hexDump(bytes, 0)
    const lines = dump.split('\n')
    expect(lines).toHaveLength(2)
    expect(lines[0]).toBe('00000000  ff fe 00 41 0a 80 7f 20  00 01 02 03 04 05 06 07')
    expect(lines[1]).toBe('00000010  e2 82')
    // every byte is recoverable from the dump
    const back = lines.flatMap((l) => l.slice(10).trim().split(/\s+/).map((h) => parseInt(h, 16)))
    expect(back).toEqual([...bytes])
    // a later window carries its absolute offset
    expect(hexDump(Uint8Array.from([1]), 65536)).toBe('00010000  01')
  })
})

describe('pickDefaultLanguage (the admin’s browser language when supported, else en)', () => {
  it.each([
    ['de-DE', 'de'],
    ['en-US', 'en'],
    ['pt-BR', 'pt'],
    ['zh-Hant-TW', 'zh'],
    ['FR', 'fr'],
    ['nb-NO', 'nb'],
    ['no', 'nb'],
    ['xx-YY', 'en'],
    ['', 'en'],
    [undefined, 'en'],
    [null, 'en'],
  ])('%s → %s', (input, want) => {
    expect(pickDefaultLanguage(input)).toBe(want)
  })

  it('only ever picks a language of the server’s closed set', () => {
    for (const l of ['de', 'ja', 'he', 'xx', 'qq-ZZ']) expect(Object.keys(RECORDING_LANGUAGES)).toContain(pickDefaultLanguage(l))
  })
})

describe('localInputToRfc3339', () => {
  it('an empty input is no bound', () => {
    expect(localInputToRfc3339('')).toEqual({ ok: true, value: undefined })
    expect(localInputToRfc3339('   ')).toEqual({ ok: true, value: undefined })
  })
  it('a wall-clock time becomes an RFC 3339 instant of the viewer’s zone', () => {
    const r = localInputToRfc3339('2026-06-29 11:00')
    expect(r.ok).toBe(true)
    expect(r.ok && r.value).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/)
    expect(r.ok && r.value).toBe(new Date(2026, 5, 29, 11, 0).toISOString())
    expect(localInputToRfc3339('2026-06-29T11:00:30')).toEqual({ ok: true, value: new Date(2026, 5, 29, 11, 0, 30).toISOString() })
    expect(localInputToRfc3339(' 2026-06-29 ')).toEqual({ ok: true, value: new Date(2026, 5, 29).toISOString() })
  })
  it('garbage or a day that does not exist is an error, not a silently dropped bound', () => {
    for (const bad of ['yesterday', '2026-13-01 10:00', '2026-02-30', '2026-06-29 24:00', '2026-06-29 11:60', '29.06.2026', '2026-6-29']) {
      expect(localInputToRfc3339(bad).ok, bad).toBe(false)
    }
  })
})

describe('recordingError — every code to its named state', () => {
  const env = (status: number, code: string, extra: Record<string, unknown> = {}) =>
    new ApiError(status, 'x', `server text for ${code}`, 'req-1', { success: false, error: `server text for ${code}`, code, ...extra })

  it.each([
    [503, 'no_eligible_backend', /No local backend may receive recorded content/],
    [503, 'window_too_small', /context window/i],
    [413, 'block_too_large', /too large/i],
    [422, 'not_translatable', /no text to translate/i],
    [429, 'translate_busy', /a translation of yours is running/i],
    [502, 'translate_failed', /model did not answer/i],
    [503, 'canceled', /canceled/i],
    [503, 'audit_unavailable', /could not be audited/i],
    [429, 'quota_exceeded', /quota/i],
    [400, 'invalid_language', /language/i],
    [404, 'not_found', /not found|no block/i],
    [409, 'body_cleared', /retention cleared/i],
    [409, 'no_body', /no body/i],
  ])('%i %s', (status, code, text) => {
    const e = recordingError(env(status, code))
    expect(e.code).toBe(code)
    expect(e.text).toMatch(text)
    expect(e.requestId).toBe('req-1')
  })

  it('block_too_large names the server’s limit_runes', () => {
    const e = recordingError(env(413, 'block_too_large', { limit_runes: 256000 }))
    expect(e.text).toContain('256000')
  })

  it('an unknown code keeps the server’s text; the shared 403 has no code', () => {
    expect(recordingError(env(500, 'internal')).text).toBe('server text for internal')
    const forbidden = recordingError(new ApiError(403, 'forbidden', 'admin key required'))
    expect(forbidden.code).toBe('forbidden')
    expect(forbidden.text).toBe('admin key required')
  })
})

describe('showRecordingsLink (entry links on /gateway and the consumer page)', () => {
  it('server-admin and a recording consumer listed ⇒ the link', () => {
    expect(showRecordingsLink(caps(true), [{ record_bodies: false }, { record_bodies: true }])).toBe(true)
  })
  it('server-admin without a recording consumer ⇒ no link', () => {
    expect(showRecordingsLink(caps(true), [{ record_bodies: false }])).toBe(false)
    expect(showRecordingsLink(caps(true), [])).toBe(false)
  })
  it('a tenant-admin or member never gets the link, recording consumer or not', () => {
    expect(showRecordingsLink(caps(false, 'owner'), [{ record_bodies: true }])).toBe(false)
    expect(showRecordingsLink(caps(false, 'member'), [{ record_bodies: true }])).toBe(false)
    expect(showRecordingsLink(capabilitiesFor(null), [{ record_bodies: true }])).toBe(false)
  })
})

describe('fmtBytes', () => {
  it.each([
    [0, '0 B'],
    [1, '1 B'],
    [1023, '1023 B'],
    [1024, '1.0 KiB'],
    [1536, '1.5 KiB'],
    [65536, '64.0 KiB'],
    [1048064, '1023.5 KiB'],
    [1048576, '1.0 MiB'],
    [5 * 1024 * 1024 * 1024, '5.0 GiB'],
  ])('%i → %s', (n, want) => {
    expect(fmtBytes(n)).toBe(want)
  })
  it('null / a negative or non-finite value is a dash, never a made-up 0', () => {
    expect(fmtBytes(null)).toBe('—')
    expect(fmtBytes(-1)).toBe('—')
    expect(fmtBytes(Number.NaN)).toBe('—')
  })
})

// The gateway area's own pages show the entry links and nothing else of the
// view: importing the recordings data layer from them puts it into a chunk
// /gateway and the consumer page load for every reader (the bundler merged
// it with format.ts once — 2177 B br on /gateway, the release budget's total
// exceeded).
describe('the gateway pages import none of the recordings view', () => {
  const VIEW = /from '(?:\.\/recording-data|\.\/recordings\.svelte|\.\.\/\.\.\/lib\/api\/recordings)'/
  it('GatewayPage, ConsumerDetail, format.ts and recording-link.ts import neither the view’s data layer nor its API client', () => {
    expect(`import { x } from './recording-data'`).toMatch(VIEW)
    expect(`import { y } from '../../lib/api/recordings'`).toMatch(VIEW)
    for (const f of ['GatewayPage.svelte', 'ConsumerDetail.svelte', 'format.ts', 'recording-link.ts']) {
      expect(readFileSync(new URL(f, import.meta.url), 'utf8'), f).not.toMatch(VIEW)
    }
  })
})
