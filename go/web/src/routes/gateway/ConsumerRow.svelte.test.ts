// The recording switch in the consumer views (issue #571): a consumer with
// record_bodies shows a "recording" badge in its list row and says so in the
// detail's policy card (recordingText); one without shows neither, and its
// kind cell is the markup it always was (the visual baselines). The flag is
// the server's — the views only render it. Server-rendered and asserted on
// the markup (BlockDetail test pattern: mount() is unavailable in the
// node-only vitest env).

import { describe, expect, it } from 'vitest'
import { render } from 'svelte/server'
import ConsumerRow from './ConsumerRow.svelte'
import { consumerOf } from './forms.fixtures'
import { recordingText } from './format'

function row(recordBodies: boolean): string {
  const c = consumerOf('rec', { record_bodies: recordBodies })
  return render(ConsumerRow, {
    props: {
      consumer: c,
      crossTenant: false,
      reactivatable: false,
      busy: false,
      onrevoke: () => {},
      ondeactivate: () => {},
      onreactivate: () => {},
    },
  }).body
}

describe('consumer recording', () => {
  it('a recording consumer carries the badge in its list row', () => {
    const markup = row(true)
    expect(markup).toContain('data-consumer-recording')
    expect(markup).toMatch(/>recording</)
  })

  it('a consumer without the switch carries none, its kind cell as it always was', () => {
    const markup = row(false)
    expect(markup).not.toContain('data-consumer-recording')
    expect(markup).not.toMatch(/>recording</)
    // Nothing but the if-block's own (invisible) comment anchors follows
    // the kind badge in its cell.
    expect(markup).toMatch(/<td><span class="badge[^"]*"[^>]*>app<\/span>(<!--\[-1--><!--\]-->)?<\/td>/)
  })

  it('the detail text names what is held, where, and who reads it', () => {
    expect(recordingText).toMatch(/in full/)
    expect(recordingText).toMatch(/credentials/)
    expect(recordingText).toMatch(/context_llm_recordings/)
    expect(recordingText).toMatch(/database/)
  })
})
