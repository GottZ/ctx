<script lang="ts">
  // Policy form of a model alias (design/05 §3.3 `policy`; wave W05-13 — RC-D
  // C5). Contract (S2): one labelled input per catalog.policyKeys(kind)
  // (max_output_tokens, default_max_tokens, max_n, timeout_s, reasoning,
  // vision, context_length, embedding.*, rerank.*); validation is the
  // server's fail-closed parser — a 422 `fields` entry (catalog.fieldErrors)
  // renders at its input (aria-invalid + message). Bindable `policy`.
  //
  // RC-D S1: contract stub — renders a placeholder.
  import type { ModelAliasKind } from '../../lib/api/types'

  interface Props {
    kind: ModelAliasKind
    policy: Record<string, unknown>
    /** field → message from a 422 */
    errors?: Record<string, string>
    disabled?: boolean
  }
  // S1: only the kind renders; S2 renders policyKeys(kind) bound to `policy`.
  let { kind, policy = $bindable() }: Props = $props()
</script>

<p data-testid="rcd-stub" data-policy-keys={Object.keys(policy ?? {}).length}>Policy form for {kind} aliases is not wired yet (RC-D W05-13).</p>
