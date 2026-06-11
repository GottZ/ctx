<script lang="ts">
  import AreaPlaceholder from '../AreaPlaceholder.svelte'
  import { session } from '../../lib/auth.svelte'
</script>

<AreaPlaceholder
  title="Settings"
  description="Every runtime setting becomes editable here — widgets render
    generically from the GET /api/settings metadata catalog, with source,
    mutability and validation surfaced inline."
  ships="Ships with wave W4; the backend-pool and secrets editors follow in W5."
>
  {#if !session.admin}
    <p class="readonly" role="status">
      read-only key — settings are admin-gated; this area will render with all
      widgets disabled.
    </p>
  {/if}
</AreaPlaceholder>

<style>
  .readonly {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    color: var(--warn);
    font-size: 0.875rem;
  }
</style>
