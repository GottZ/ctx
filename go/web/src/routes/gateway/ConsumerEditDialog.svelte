<script lang="ts">
  // Edit a model consumer's policy (design/05 §4.5 Feld-Tier-UX, §4.6; wave
  // W05-07 — RC-D C5). Contract (S2): one Modal (lazy chunk like
  // ConsumerCreateDialog) with label, description, tenant_scope, models /
  // "all offered", prompt sensitivity, lane (LaneSelect), network allowlist —
  // every control gated by policy.editTiers(consumer, caps): lower
  // sensitivities disabled "server-admin only" for a tenant-admin, widening
  // models / CIDRs / scope disabled without mintModelKeys, no dialog at all for
  // a ctx-peer row below server-admin. Save sends policy.buildUpdatePatch
  // (changed fields only) through model.update; a refusal (403 "admin key
  // required", 400 field, 409, 429) renders role="alert" IN the dialog and
  // the dialog stays open — never a success toast.
  //
  // RC-D S1: contract stub — renders a placeholder, sends nothing.
  import type { ConsumerView } from '../../lib/api/types'
  import Modal from '../../lib/ui/Modal.svelte'
  import type { ConsumersModel } from './consumers.svelte'

  interface Props {
    model: ConsumersModel
    consumer: ConsumerView
    onclose: () => void
  }
  // S1: only what the placeholder renders is read; S2 uses `model`.
  let { consumer, onclose }: Props = $props()

  let dialogEl: HTMLDialogElement | undefined = $state()
</script>

<Modal bind:dialogEl width="40rem" ariaLabelledby="consumer-edit-title" onclose={onclose}>
  <header>
    <h2 id="consumer-edit-title">Edit policy — {consumer.label}</h2>
  </header>
  <p data-testid="rcd-stub">Policy editing is not wired yet (RC-D W05-07).</p>
</Modal>
