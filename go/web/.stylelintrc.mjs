// Stylelint token gate (design 05-§4.4, wave Q4) — G-A: token compliance is
// enforced per construction, not per review. Svelte <style> blocks are parsed
// via stylelint-config-html/svelte (customSyntax postcss-html).
//
// RATCHET (design 05-§4.4 + §7): Q4 arms the colour/shadow/z/radius/motion
// families globally — Q3 made them literal-free, this config freezes that
// zero state. The TYPO families (font-size, font-family, font-weight,
// line-height, letter-spacing) join the property list with the Q5–Q8
// migration waves; arming them today would paint the tree red with ~311
// pre-existing literals and break the pausability invariant.
//
// Exceptions policy (design 05-§5.6): a literal needs an inline
// `stylelint-disable` WITH a justification comment — review-visible, never
// config-wide. The inline-style gate (style attributes / directives /
// expressions) is NOT stylelint's job: postcss-html only sees <style>
// blocks; `lint/inline-gate.mjs` covers the attribute channel.

/** Q4 ratchet scope — every non-typo family (design 05-§7, Q4 row). */
const strictValueProps = [
  '/color$/', // color, background-color, border-color, outline-color, …
  'background',
  'fill',
  'stroke',
  'box-shadow',
  'z-index',
  'border-radius',
  'transition-duration',
  'animation-duration',
]

// Q5 ratchet extension (design 05-§7, Q5 row): the TYPO families go strict —
// but ONLY on the migrated paths. Q6/Q7 WIDEN the flächen (paths are ADDED to
// the list below, the ratchet only grows), Q8 arms them globally. font-family
// is deliberately NOT armed here (design 05-§4.4 lists it for the Q8 global
// pass; the wave rows enumerate font-size/line-height/font-weight/letter-spacing
// only).
const typoProps = ['font-size', 'font-weight', 'line-height', 'letter-spacing']

// Files whose typo declarations are migrated onto the scale. The strict gate is
// scoped to exactly this set: a font-size literal here is red, the same literal
// in a not-yet-migrated file (e.g. src/routes/settings|status/** — the Q8
// fläche) still passes — the pausability invariant. Whole-dir globs for lib/ui +
// lib/components mirror the Q5 brief; the Shell/Login files are explicit.
const migratedPaths = [
  // Q5 — Kern-Fläche (Shell + Login + lib/ui + lib/components).
  'src/App.svelte',
  'src/AppShell.svelte',
  'src/NavRail.svelte',
  'src/NavDrawer.svelte',
  'src/Wordmark.svelte',
  'src/Login.svelte',
  'src/lib/ui/**/*.svelte',
  'src/lib/components/**/*.svelte',
  // Q6 — Corpus-Flächen (design 05-§7 Q6 row: /blocks + /chat + /graph).
  'src/routes/blocks/**/*.svelte',
  'src/routes/chat/**/*.svelte',
  'src/routes/graph/**/*.svelte',
  // Q7 — Verwaltungs-Flächen (design 05-§7 Q7 row: /admin + /tenant). The
  // /admin dir covers AdminPage + /admin/tenants/:id (TenantDetail) +
  // TenantCreateDialog/CorpusMaintenance/QuotaForm/ScopeMap/TenantProvision;
  // /tenant covers TenantPage + KeyCreateDialog + the self-service cards.
  'src/routes/admin/**/*.svelte',
  'src/routes/tenant/**/*.svelte',
]

// Keywords legit as literals across strict props (shared by base + Q5 override).
// NB: '1' is intentionally DROPPED from the Q5 override so `line-height: 1`
// must carry var(--lh-solid) (no strict non-typo prop in the migrated set uses
// a literal 1); 'normal' is added for line-height/letter-spacing/font-weight.
const baseIgnoreValues = ['currentColor', 'transparent', 'inherit', 'initial', 'unset', 'none', 'auto', '0', '1', '50%']
const typoIgnoreValues = ['currentColor', 'transparent', 'inherit', 'initial', 'unset', 'none', 'auto', '0', '50%', 'normal']

export default {
  extends: ['stylelint-config-html/svelte'],
  plugins: ['stylelint-declaration-strict-value'],
  rules: {
    'scale-unlimited/declaration-strict-value': [
      strictValueProps,
      // Keywords that are legitimately literal (no token can carry them).
      { ignoreValues: baseIgnoreValues },
    ],
    // Spacing is deliberately NOT strict-value-linted (189 legitimate 0/auto
    // + hairlines would drown the signal — design 05-§4.4). Instead `gap`
    // gets a tight allowed-list: the space scale, 0, and 1–3px hairlines.
    // The three rem literals are Q3 leftovers (BlocksPage 0.2rem, FilterPanel
    // 0.3rem, SearchBox/ToolCallCard/corpus 0.15rem) — documented ratchet
    // debt, to be migrated onto the scale under the Q5+ baseline net; new
    // off-scale values stay red.
    'declaration-property-value-allowed-list': {
      gap: [
        /^(?:0|1px|2px|3px|0\.15rem|0\.2rem|0\.3rem|var\(--space-[0-9]\))(?: (?:0|1px|2px|3px|0\.15rem|0\.2rem|0\.3rem|var\(--space-[0-9]\)))?$/,
      ],
    },
    // Token namespace contract: every custom property belongs to a declared
    // family (design 05-§4.4 config block).
    'custom-property-pattern':
      '^(surface|text|accent|ok|warn|danger|border|graph|dot|font|space|radius|label|rail|shell|measure|detail|session|z|backdrop|shadow|dur|ease|focus|fs|lh|fw|track|type|sync)-?',
    'custom-property-no-missing-var-function': true,
  },
  // Ratchet slots: typo families are armed per migration wave (Q5–Q8). Q5
  // scopes them to the migrated paths via this override — stylelint replaces the
  // rule wholesale for matched files, so the prop list carries BOTH the Q4
  // non-typo families AND the Q5 typo families (dropping either would un-arm it
  // on these files). Q8 folds typoProps into the global strictValueProps and
  // retires this override (plus the app.css root-15px scoped ignoreValues).
  overrides: [
    {
      files: migratedPaths,
      rules: {
        'scale-unlimited/declaration-strict-value': [
          [...strictValueProps, ...typoProps],
          { ignoreValues: typoIgnoreValues },
        ],
      },
    },
  ],
}
