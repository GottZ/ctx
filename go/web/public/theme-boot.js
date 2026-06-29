/* ctx theme boot — render-blocking CLASSIC script (design 02-theme.md §1/§8).
 *
 * MUST NOT be type=module: modules are implicitly deferred and run AFTER HTML
 * parsing, i.e. too late to set data-theme before first paint → dark flash.
 * This is a plain synchronous classic script, loaded as the first <head> child.
 *
 * Served from /theme-boot.js — Vite copies web/public/ verbatim to the dist
 * root (same as favicon.svg), so it ships un-bundled and same-origin. Being
 * external + non-inline, it satisfies the deployed `script-src 'self'` CSP
 * (web.go:46, "no inline scripts") with ZERO backend change.
 *
 * Deliberate mini-duplication of ThemeController.resolveTheme (theme.svelte.ts):
 * this runs before the bundle loads, so it cannot import the controller. Keep
 * the resolve rule in sync — stored pref → concrete light|dark, `system`/none
 * via matchMedia, dark as the no-signal default (matches tokens.css :root).
 */
(function () {
  try {
    var pref = null
    try {
      pref = localStorage.getItem('ctx.theme')
    } catch (e) {
      /* localStorage blocked (private mode / cookies off) — treat as system. */
    }

    var resolved
    if (pref === 'light' || pref === 'dark') {
      resolved = pref
    } else {
      var dark = true /* no OS signal → dark, the attributeless :root default. */
      try {
        dark = window.matchMedia('(prefers-color-scheme: dark)').matches
      } catch (e) {
        /* matchMedia unavailable — keep the dark default. */
      }
      resolved = dark ? 'dark' : 'light'
    }

    document.documentElement.setAttribute('data-theme', resolved)
  } catch (e) {
    /* Never throw from the head. On any failure, leave data-theme unset and let
     * the attributeless :root fall back to its color-scheme:dark default. */
  }
})()
