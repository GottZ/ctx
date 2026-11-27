// The entry links to the recordings view (issue #575) on /gateway and the
// consumer page. A module of its own: the gateway pages that show the links
// import this alone, never the recordings view's data layer
// (recording-data.ts, lib/api/recordings.ts), which stays in the recordings
// chunks.

import type { Capabilities } from '../../lib/auth/capabilities'

/**
 * The entry links to the recordings (on /gateway and the consumer page): a
 * server-admin, and at least one of the listed consumers records. Nobody else
 * gets a link — the actions are server-admin only and the guard keeps the
 * route from everybody else.
 */
export function showRecordingsLink(caps: Capabilities, consumers: ReadonlyArray<{ record_bodies: boolean }>): boolean {
  return caps.tier === 'server-admin' && consumers.some((c) => c.record_bodies)
}
