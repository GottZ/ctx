<script lang="ts">
  // Alias catalog /gateway/models (design/05 §3.3, §4.6 ModelsPage; wave
  // W05-13 — RC-D C5). Contract (S2): heading "Model catalog"; the table of
  // AliasesModel.aliases (scope, name, kind, role, sensitivity, enabled) —
  // write controls only where catalog.rowControls allows (never on `_global`
  // rows for a tenant-admin); "+ New alias" opens the editor (name / kind
  // free, scope per tier, sensitivity per catalog.aliasSensitivityOptions,
  // AliasPolicyForm, enabled explicit); "Edit" opens it with name and kind
  // read-only; lowering the sensitivity opens a danger confirm first;
  // server-admin only: "Suggest from pool" (drafts, enabled:false) and a
  // per-row chain preview (model-alias-preview, C7 shape).
  //
  // RC-D S1: contract stub — the store delivers empty data; a placeholder.
  import { onMount } from 'svelte'
  import { AliasesModel } from './aliases.svelte'

  const model = new AliasesModel()
  onMount(() => {
    void model.load()
  })
</script>

<section class="models" aria-labelledby="models-title">
  <h1 id="models-title">Model catalog</h1>
  <p data-testid="rcd-stub">The alias catalog is not wired yet (RC-D W05-13).</p>
</section>
