<script lang="ts">
  // The ctx wordmark with its terminal cursor — shared by login, boot splash
  // and the top bar.
</script>

<span class="wordmark">ctx<span class="cursor">_</span></span>

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
