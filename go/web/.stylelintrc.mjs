// Stylelint token gate (design 05-§4.4, waves Q4–Q8) — G-A: token compliance is
// enforced per construction, not per review. Svelte <style> blocks are parsed
// via stylelint-config-html/svelte (customSyntax postcss-html).
//
// RATCHET (design 05-§4.4 + §7): Q4 armed the colour/shadow/z/radius/motion
// families globally; Q5–Q7 armed the TYPO families (font-size, font-family,
// font-weight, line-height, letter-spacing) on a growing set of migrated paths
// via an override. Q8 is the GLOBAL close of the typo chain: every fläche is
// migrated (/settings + /status + Wurzeln + lib/theme + em-Konsolidierung), so
// the typo families fold into the global prop list and the per-path override is
// retired. The only surviving off-scale font-size literal is the rem-scale root
// anchor (app.css html { font-size: 15px }), allowed via a scoped ignoreValues
// override below (design 05-§4.4: "ignoreValues +'15px' NUR für app.css-Root").
// The two em cases (code / MessageBubble) now run through var(--fs-code-rel).
//
// Exceptions policy (design 05-§5.6): a literal needs an inline
// `stylelint-disable` WITH a justification comment — review-visible, never
// config-wide. The inline-style gate (style attributes / directives /
// expressions) is NOT stylelint's job: postcss-html only sees <style>
// blocks; `lint/inline-gate.mjs` covers the attribute channel.

/** Global ratchet scope — all colour/box/motion families (Q4) + all typo
 *  families incl. font-family (Q8 global close, design 05-§4.4 + §7 Q8 row). */
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
  'font-size',
  'font-family',
  'font-weight',
  'line-height',
  'letter-spacing',
]

// Keywords legit as literals across ALL strict props. NB: '1' is intentionally
// ABSENT so `line-height: 1` must carry var(--lh-solid) (no strict prop uses a
// bare literal 1 — z-index/radius carry their own tokens); 'normal' covers the
// line-height/letter-spacing/font-weight/font-family keyword. Font-family stacks
// resolve to var(--font-ui)/var(--font-mono); a bare stack literal is now red.
const ignoreValues = ['currentColor', 'transparent', 'inherit', 'initial', 'unset', 'none', 'auto', '0', '50%', 'normal']

export default {
  extends: ['stylelint-config-html/svelte'],
  plugins: ['stylelint-declaration-strict-value'],
  rules: {
    'scale-unlimited/declaration-strict-value': [strictValueProps, { ignoreValues }],
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
  // Scoped exception: app.css html { font-size: 15px } is the rem-scale root
  // anchor (design 05-§4.3/§4.4) — the single allowed font-size literal in the
  // tree. The override replaces the rule wholesale for this file, so it carries
  // the full prop list + the base ignoreValues plus '15px'.
  overrides: [
    {
      files: ['src/app.css'],
      rules: {
        'scale-unlimited/declaration-strict-value': [strictValueProps, { ignoreValues: [...ignoreValues, '15px'] }],
      },
    },
  ],
}
