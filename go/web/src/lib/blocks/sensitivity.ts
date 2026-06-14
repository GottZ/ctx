// Sensitivity badge helper for the block-workbench W6 list + detail badge.
// Pure function so vitest covers it without a DOM (lib/blocks/edit.ts pattern).
//
// The four trust-gate levels (backends/trust.go: credentials rank 3 … public
// rank 0) map onto the semantic token trio (styles/tokens.css: danger/warn/
// ok) + accent — the `tone` is a CSS-class suffix, exactly the established
// `<span class="badge {variant}">` pattern (SettingField.svelte, BackendsTile).
//
// FAIL-CLOSED: an unknown/empty level is treated as credentials (the strictest)
// — an unclassified block defaults to credentials server-side (fail-closed
// trust gating), so the badge must not under-state an unlabelled block.
//
/** A sensitivity badge descriptor: a human label + a token-tone CSS suffix. */
export interface SensitivityBadge {
  /** Human level name shown in the badge (e.g. 'credentials'). */
  label: string
  /** Token tone — a CSS-class suffix: 'danger' | 'warn' | 'accent' | 'ok'. */
  tone: string
}

/**
 * The credentials (danger) badge — the fail-closed default for an unknown or
 * absent level: an unclassified block defaults to credentials server-side, so
 * the badge must never under-state it.
 */
const CREDENTIALS_BADGE: SensitivityBadge = { label: 'credentials', tone: 'danger' }

// Level → token tone. The four trust-gate levels (backends/trust.go) map onto
// the semantic token trio (styles/tokens.css: danger/warn/ok) + accent.
const TONE_BY_LEVEL: Record<string, string> = {
  credentials: 'danger',
  personal: 'warn',
  internal: 'accent',
  public: 'ok',
}

/**
 * Map a sensitivity level to its badge descriptor. credentials→danger,
 * personal→warn, internal→accent, public→ok; an unknown/empty level
 * fail-closes to the credentials (danger) badge.
 */
export function sensitivityBadge(level: string): SensitivityBadge {
  const tone = TONE_BY_LEVEL[level]
  if (tone === undefined) return CREDENTIALS_BADGE
  return { label: level, tone }
}
