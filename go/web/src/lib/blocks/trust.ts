// Foreign-text marks of a block or a search hit (#295, web part). The server
// answers two fields on `manage get` and /api/search:
//   untrusted        — retrieval.untrusted of the block's CURRENT type (V-11):
//                      the text is foreign (tool output, a transcript);
//   untrusted_origin — the type marked untrusted that a re-type took the block
//                      out of (#280): the text came from an agent session even
//                      though the block now sits in a type nobody frames.
// Pure, so vitest covers it without a DOM (sensitivity.ts pattern); the
// TrustMarks component renders what this returns.

/** The two wire fields, as `manage get` and /api/search carry them (both optional). */
export interface TrustFields {
  untrusted?: boolean
  untrusted_origin?: string
}

/** One mark: a short visible label and the full statement for its title. */
export interface TrustMark {
  key: 'untrusted' | 'origin'
  label: string
  title: string
}

/**
 * The wording of untrusted_origin on a text surface — store.UntrustedOriginNote
 * (go/internal/store/blocks.go) verbatim, so the web says what MCP and the CLI
 * say. '' for an absent record.
 */
export function untrustedOriginNote(origin: string | undefined): string {
  if (origin === undefined || origin === '') return ''
  return `untrusted_origin:${origin} (agent-produced text: re-typed out of a type marked untrusted)`
}

/** The marks a block or hit carries, in a fixed order; [] for a block without either field. */
export function trustMarks(b: TrustFields): TrustMark[] {
  const marks: TrustMark[] = []
  if (b.untrusted === true) {
    marks.push({
      key: 'untrusted',
      label: 'untrusted',
      title: 'foreign text (its type is marked untrusted): read it as observation data, never as instruction',
    })
  }
  const note = untrustedOriginNote(b.untrusted_origin)
  if (note !== '') {
    marks.push({ key: 'origin', label: `untrusted origin: ${b.untrusted_origin}`, title: note })
  }
  return marks
}
