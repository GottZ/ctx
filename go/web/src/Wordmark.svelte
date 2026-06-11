<script lang="ts">
  // The ctx wordmark with its terminal cursor — shared by login, boot splash
  // and the top bar.
</script>

<span class="wordmark">ctx<span class="cursor">_</span></span>

<style>
  .wordmark {
    font-family: var(--font-mono);
    font-weight: 600;
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
