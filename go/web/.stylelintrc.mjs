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

export default {
  extends: ['stylelint-config-html/svelte'],
  plugins: ['stylelint-declaration-strict-value'],
  rules: {
    'scale-unlimited/declaration-strict-value': [
      strictValueProps,
      {
        // Keywords that are legitimately literal (no token can carry them).
        ignoreValues: [
          'currentColor',
          'transparent',
          'inherit',
          'initial',
          'unset',
          'none',
          'auto',
          '0',
          '1',
          '50%',
        ],
      },
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
  // Ratchet slots: typo families are armed per migration wave (Q5–Q8) by
  // extending strictValueProps above; per-path overrides go here if a wave
  // needs a scoped ignoreValues (e.g. the app.css root 15px scale anchor).
  overrides: [],
}
