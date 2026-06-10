<script lang="ts">
  // Toolchain shell only (G06). Router, login and the Settings / Status /
  // Graph / Chat areas arrive with the SPA-shell waves (G20+).
</script>

<main>
  <h1>ctx</h1>
  <p class="tagline">Your AI's save game.</p>
  <p>
    The admin UI shell is built and embedded into the ctxd binary.
    Settings, Status, Graph and Chat ship in upcoming waves.
  </p>
  <p class="hint">All <code>/api/*</code> endpoints are live on this origin.</p>
</main>

<style>
  main {
    max-width: 40rem;
    margin: 18vh auto 0;
    padding: 0 var(--space-3);
  }
  h1 {
    font-size: 2.25rem;
    letter-spacing: 0.04em;
    margin: 0 0 var(--space-1);
    color: var(--accent);
  }
  .tagline {
    color: var(--text-dim);
    margin: 0 0 var(--space-3);
  }
  .hint {
    color: var(--text-dim);
    font-size: 0.875rem;
  }
  code {
    font-family: var(--font-mono);
    background: var(--surface-1);
    padding: 0.1em 0.35em;
    border-radius: 4px;
  }
</style>
