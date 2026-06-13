<script lang="ts">
  // Write-only secret value input (design 04-§3.5): type=password, no
  // autocomplete, and the field clears itself after a successful submit so the
  // plaintext never lingers in the DOM. onsubmit returns true on success
  // (clear) / false on failure (keep the value so the user can retry).
  let {
    submitLabel,
    busy = false,
    disabled = false,
    onsubmit,
  }: {
    submitLabel: string
    busy?: boolean
    disabled?: boolean
    onsubmit: (value: string) => Promise<boolean>
  } = $props()

  let value = $state('')

  async function submit(): Promise<void> {
    if (value === '' || busy || disabled) return
    const ok = await onsubmit(value)
    if (ok) value = ''
  }
</script>

<form
  class="redacted"
  onsubmit={(e) => {
    e.preventDefault()
    void submit()
  }}
>
  <input
    type="password"
    autocomplete="off"
    spellcheck="false"
    placeholder="value (write-only — never shown again)"
    bind:value
    disabled={busy || disabled}
  />
  <button type="submit" disabled={value === '' || busy || disabled}>{busy ? '…' : submitLabel}</button>
</form>

<style>
  .redacted {
    display: flex;
    gap: var(--space-2);
    align-items: center;
  }
  input {
    flex: 1;
    min-width: 0;
    font-family: var(--font-mono);
    font-size: 0.85rem;
    padding: var(--space-1) var(--space-2);
  }
  input:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  button {
    font-size: 0.78rem;
    padding: var(--space-1) var(--space-2);
    white-space: nowrap;
  }
</style>
