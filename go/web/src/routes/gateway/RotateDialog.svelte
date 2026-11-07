<script lang="ts">
  // Rotate a consumer's key (design/05 §4.6 RotateDialog, §5 B-20; wave W05-08
  // — RC-D C5). Contract (S2): required question "Reason" — radio "Leak /
  // suspicion — revoke the old key now" PRESELECTED (rotate.defaultRotateChoice
  // = grace 0) or "Planned change" with the grace select 1 h / 24 h / 7 d
  // (preselected = rotation_grace_s, rotate.graceOptions); submit sends
  // rotate.rotateSpec through model.rotate, then the SECOND reveal-once pass:
  // RevealOnceKey (unchanged) with the new plaintext, `revealKey = ''` after
  // tick(), wiped on ack / Esc — never in the model, storage, URL or console.
  // A refusal (403 peer / no self-service, 409 inactive, 400 grace) stays at
  // the dialog as role="alert".
  //
  // RC-D S1: contract stub — renders a placeholder, sends nothing.
  import type { ConsumerView } from '../../lib/api/types'
  import Modal from '../../lib/ui/Modal.svelte'
  import type { ConsumersModel } from './consumers.svelte'

  interface Props {
    model: ConsumersModel
    consumer: ConsumerView
    /** whoami model_api.rotation_grace_s (the planned-change default). */
    rotationGraceS: number | null
    onclose: () => void
  }
  // S1: only what the placeholder renders is read; S2 uses model + rotationGraceS.
  let { consumer, onclose }: Props = $props()

  let dialogEl: HTMLDialogElement | undefined = $state()
</script>

<Modal bind:dialogEl width="36rem" ariaLabelledby="consumer-rotate-title" onclose={onclose}>
  <header>
    <h2 id="consumer-rotate-title">Rotate key — {consumer.label}</h2>
  </header>
  <p data-testid="rcd-stub">Key rotation is not wired yet (RC-D W05-08).</p>
</Modal>
