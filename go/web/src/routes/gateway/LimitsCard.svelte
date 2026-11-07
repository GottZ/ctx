<script lang="ts">
  // Limits card of one consumer (RC-D C6, wave W05-12; design/05 §4.5/§4.6):
  // per op the `server`, `tenant`, `defaults` and `effective` values
  // (gateway-limits-get) and the form of the caller's tier
  // (limits-form.ts limitFieldSpec): a tenant-admin's input maximum is
  // server(d), there is no "unlimited" control, an empty field reads
  // "inherits …"; a 422 shows its message at the field. State: LimitsModel.
  //
  // RC-D S1: contract stub — renders nothing.
  let { consumerId }: { consumerId: string } = $props()
  // S1: the props are read nowhere yet; the effect keeps them referenced.
  $effect(() => void [consumerId])
</script>
