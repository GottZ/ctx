// The two operator-facing texts of the llmlog detail card. They live in their
// own module rather than inside LlmlogTable.svelte because vitest runs
// environment:'node' and never mounts a component — a wording inside the
// component is unreachable by every gate this repo has. These are privacy
// affordances; they get a red/green like the model classes do.
//
// Source of the states: go/internal/handler/llmlog.go
// (bodyPresent/bodySealed/bodyEvicted/bodyBodyless).

// bodyStateReason is the human-readable reason a body is ABSENT. Empty string
// for 'present' (the bodies speak for themselves) and for any state this build
// does not know — a future backend state renders no text rather than a wrong
// one.
export function bodyStateReason(state: string): string {
  if (state === 'sealed') return 'sealed — credentials-class call, prompt/reply never stored'
  if (state === 'evicted') return 'evicted — bodies removed by retention'
  if (state === 'bodyless')
    return 'bodyless — this pipeline never records prompt/reply (embed, translate, rejection lines)'
  return ''
}

// unsealedCredentialsNote is the counterpart for a body that IS shown: a
// credentials-class row rendering its prompt/reply is the tenant devmode
// override at work (config key tenant.devmode), never the default. Without the
// note the reader would see the hottest class rendered exactly like an
// 'internal' one and could take the class for harmless — the sensitivity is
// already on the card, but a raw label is not a warning.
//
// Empty for every other class and for any state that shows no bodies, so the
// note can only ever appear next to plaintext it actually describes.
export function unsealedCredentialsNote(state: string, sensitivity: string): string {
  if (state !== 'present' || sensitivity !== 'credentials') return ''
  return 'credentials-class · unsealed by tenant devmode'
}
