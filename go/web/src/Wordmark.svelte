<script lang="ts">
  // The ctx wordmark with its terminal cursor — shared by login, boot splash
  // and the top bar. The cursor is aria-hidden: it is a decorative signature
  // (the Q12 pulse), never part of the accessible link name. Without it the
  // visibility-blink toggled the cursor in and out of the a11y name, so the
  // committed aria baselines had frozen it inconsistently ("ctx" on most pages,
  // "ctx_" on status/admin/settings-backends); aria-hidden makes the name a
  // deterministic "ctx" everywhere (Q12 refreshes the three stale entries).
</script>

<span class="wordmark">ctx<span class="cursor" aria-hidden="true">_</span></span>

<style>
  .wordmark {
    font-family: var(--font-mono);
    font-weight: var(--fw-semibold);
    /* stylelint-disable-next-line scale-unlimited/declaration-strict-value -- Wordmark-Signatur-Tracking: die Typo-Skala definiert nur --track-1 (0.01em); die 0.04em-Weite ist das Signatur-Element (design 05-§4.9). Kein Snap in Q5 (pixel-neutral); eine eigene Tracking-Stufe entscheidet E1/Q11. */
    letter-spacing: 0.04em;
    color: var(--accent);
    user-select: none;
  }
  .cursor {
    /* Signatur-Element (design 05-§4.9 "spend your boldness in one place"): der
       blinkende Terminal-Cursor ist der EINE orchestrierte Signatur-Puls der UI.
       Q12 haertet ihn, statt ihn neu zu animieren: aria-hidden (oben) macht den
       Link-Namen deterministisch, und die Q12-reduced-motion-walk beweist die
       Abschaltbarkeit (lokaler Guard unten + globaler app.css-Guard). Der
       Rhythmus bleibt bewusst HEAD-identisch (steps(2,start) = diskret, zwei
       Repaints/Zyklus): eine mehrstufige/kontinuierliche Variante wurde
       verworfen, weil ihr 60fps-Repaint die parallele @visual-Erfassung der
       virtualisierten /issues-Liste ueber das 0-Pixel-Gate kippte (unter CI-Last
       reproduzierbar rot; container-verifiziert). Ein zweites Signatur-Element
       kommt NICHT dazu — der MessageBubble-Streaming-Cursor ist ein funktionaler
       Status-Indikator, keine Marke. */
    animation: blink 1.2s steps(2, start) infinite;
  }
  @keyframes blink {
    to {
      visibility: hidden;
    }
  }
  @media (prefers-reduced-motion: reduce) {
    .cursor {
      animation: none;
    }
  }
</style>
