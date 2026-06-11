<script lang="ts">
  import AreaPlaceholder from '../AreaPlaceholder.svelte'
</script>

<AreaPlaceholder
  title="Status"
  description="The live dashboard: health, backend pool, dream queue and LLM
    telemetry — fed by the process-wide status collector, polled first and
    pushed over SSE later."
  ships="Ships with wave W6 (poll) and W7 (SSE live updates)."
/>
