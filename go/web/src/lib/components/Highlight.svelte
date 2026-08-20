<script lang="ts">
  // Text with fuzzy-match highlight ranges ([start, end) pairs from
  // lib/fuzzy's highlightRanges). Renders <mark> segments inline; an empty
  // ranges list renders the plain text with zero extra nodes.

  let { text, ranges = [] }: { text: string; ranges?: Array<[number, number]> } = $props()

  interface Segment {
    text: string
    hit: boolean
  }

  const segments = $derived.by((): Segment[] => {
    if (ranges.length === 0) return [{ text, hit: false }]
    const out: Segment[] = []
    let pos = 0
    for (const [start, end] of ranges) {
      if (start > pos) out.push({ text: text.slice(pos, start), hit: false })
      out.push({ text: text.slice(start, end), hit: true })
      pos = end
    }
    if (pos < text.length) out.push({ text: text.slice(pos), hit: false })
    return out
  })
</script>

{#each segments as seg, i (i)}{#if seg.hit}<mark>{seg.text}</mark>{:else}{seg.text}{/if}{/each}

<style>
  mark {
    background: var(--accent-dim);
    color: var(--accent);
    border-radius: var(--radius);
    padding: 0;
  }
</style>
