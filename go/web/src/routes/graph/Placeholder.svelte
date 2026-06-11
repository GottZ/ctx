<script lang="ts">
  import AreaPlaceholder from '../AreaPlaceholder.svelte'
</script>

<AreaPlaceholder
  title="Graph"
  description="The dream-link graph viewer docks here: budgeted k-hop ego
    subgraphs from GET /api/graph/ego (live since G07), rendered client-side —
    the server never ships the full graph."
  ships="Ships with F5 (sigma.js + graphology, lazy-loaded on entry)."
/>
