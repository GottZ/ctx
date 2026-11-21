// #432: the seed's keys (the per-run admin key, the tenants' owner keys) go to
// CTX_E2E_BASE_URL and nowhere else. fetch follows a redirect by default and
// keeps a custom header such as X-Context-Key on it, to any host; the target
// gate checks only the base URL's host. The throwaway ctxd never redirects
// /api/*, so the seed follows no redirect at all — one is a sign of another
// target, refused like any other gate failure. Each case runs runSeed against
// a local server that redirects; the second server must see no request.

import { createServer, type IncomingMessage, type ServerResponse } from 'node:http'
import type { AddressInfo } from 'node:net'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

const KEY = 'seed-redirect-fixture-admin-key-0123456789abcdef'
const RUN_ID = 'seed-redirect-test'

interface Served {
  url: string
  seen: string[] // "METHOD path key=<present|absent>" per request
  close: () => Promise<void>
}

function serve(handle: (req: IncomingMessage, res: ServerResponse, self: Served) => void): Promise<Served> {
  return new Promise((resolve) => {
    const self: Served = { url: '', seen: [], close: async () => {} }
    const srv = createServer((req, res) => {
      self.seen.push(`${req.method} ${req.url} key=${req.headers['x-context-key'] === undefined ? 'absent' : 'present'}`)
      req.resume()
      req.on('end', () => handle(req, res, self))
    })
    srv.listen(0, '127.0.0.1', () => {
      self.url = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`
      self.close = () => new Promise((done) => srv.close(() => done()))
      resolve(self)
    })
  })
}

function json(res: ServerResponse, status: number, body: unknown): void {
  res.writeHead(status, { 'content-type': 'application/json' })
  res.end(JSON.stringify(body))
}

function redirect(res: ServerResponse, status: number, location: string): void {
  res.writeHead(status, { location })
  res.end()
}

// The handshake a throwaway ctxd answers to the per-run key.
function handshake(res: ServerResponse): void {
  json(res, 200, { success: true, admin: true, label: `e2e-bootstrap-${RUN_ID}` })
}

let runSeed: () => Promise<void>
let stateDir: string
const servers: Served[] = []

beforeAll(async () => {
  stateDir = mkdtempSync(join(tmpdir(), 'seed-redirect-'))
  // seed.ts reads the run id when it is loaded, the rest per run.
  process.env.CTX_E2E_RUN_ID = RUN_ID
  ;({ runSeed } = await import('./seed'))
})

afterEach(async () => {
  await Promise.all(servers.splice(0).map((s) => s.close()))
})

afterAll(() => {
  rmSync(stateDir, { recursive: true, force: true })
})

async function seedAgainst(home: Served): Promise<unknown> {
  process.env.CTX_E2E_LIVE = '1'
  process.env.CTX_E2E_BASE_URL = home.url
  process.env.CTX_E2E_ADMIN_KEY = KEY
  process.env.CTX_E2E_STATE_FILE = join(stateDir, 'state.json')
  return runSeed().then(
    () => undefined,
    (err: unknown) => err,
  )
}

describe('seed.ts follows no redirect with its keys (#432)', () => {
  for (const status of [302, 307]) {
    it(`refuses a ${status} of the handshake to another origin before sending it`, async () => {
      const second = await serve((_req, res) => json(res, 401, { success: false }))
      const home = await serve((req, res) => redirect(res, status, second.url + req.url))
      servers.push(second, home)

      const err = await seedAgainst(home)

      expect(second.seen).toEqual([])
      expect(String(err)).toContain('seed target-gate refusal')
    })
  }

  it('refuses a 307 of a write to another origin before sending it', async () => {
    const second = await serve((_req, res) => json(res, 400, { success: false }))
    const home = await serve((req, res) => {
      if (req.url === '/api/whoami') return handshake(res)
      redirect(res, 307, second.url + req.url)
    })
    servers.push(second, home)

    const err = await seedAgainst(home)

    expect(second.seen).toEqual([])
    expect(err).toBeInstanceOf(Error)
    expect(home.seen).toEqual(['GET /api/whoami key=present', 'POST /api/manage key=present'])
  })

  it('refuses a redirect within the origin too: the throwaway ctxd never redirects /api/*', async () => {
    const home = await serve((req, res) => {
      if (req.url === '/moved/api/whoami') return handshake(res)
      if (req.url === '/api/whoami') return redirect(res, 307, '/moved/api/whoami')
      json(res, 400, { success: false })
    })
    servers.push(home)

    const err = await seedAgainst(home)

    expect(home.seen).toEqual(['GET /api/whoami key=present'])
    expect(String(err)).toContain('seed target-gate refusal')
  })
})
