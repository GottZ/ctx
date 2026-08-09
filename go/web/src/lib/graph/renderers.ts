// Renderer-Registry (RV1): der Graph kennt MEHRERE austauschbare Renderer über
// derselben graphology-Instanz — Sigma (Bestand, Default), cosmos.gl (GPU-
// Layout+Rendering), deck.gl (2D/3D-Baukasten), three.js (2.5D). Zweck ist der
// Live-Shootout am echten Korpus (Recherche 2026-08-09, ctx 019fe63f): die
// Renderer-Frage wird empirisch am Produkt entschieden, nicht am Papier.
// Jeder Renderer ist eine Svelte-Komponente mit identischem Props-Contract
// (RendererProps) und den Instanz-Exports resetCamera()/refresh(); geladen wird
// lazy per dynamic import — nur der aktive Renderer landet im Client-Bundle.

import type { Component } from 'svelte'
import type { MultiDirectedGraph } from 'graphology'
import type { GraphFilters } from './filters'
import type { EdgeAttrs, NodeAttrs } from './graph-client'
import type { GraphPalette } from './graph-theme'

export type RendererId = 'sigma' | 'cosmos' | 'deck' | 'three'

/** Der gemeinsame Props-Contract aller Renderer-Komponenten. Sigma ignoriert
 *  graphRev (es hört selbst auf graphology-Events) und threeD (2D-only) —
 *  überzählige Props sind in Svelte 5 wirkungslos. */
export interface RendererProps {
  graph: MultiDirectedGraph<NodeAttrs, EdgeAttrs>
  filters: GraphFilters
  palette: GraphPalette
  highlightedIds?: Set<string>
  topId?: string | null
  showLabels?: boolean
  /** Merge-Revision aus GraphPage — Buffer-Renderer bauen ihre TypedArrays
   *  auf diesen Tick neu auf (graphology selbst ist bewusst nicht reaktiv). */
  graphRev?: number
  /** 3D-Perspektive an/aus — nur Renderer mit caps.threeD lesen das. */
  threeD?: boolean
  onnodeclick?: (id: string) => void
  onnodedoubleclick?: (id: string) => void
}

/** Instanz-API, die GraphPage über bind:this anspricht. */
export interface RendererExports {
  resetCamera(): void
  refresh(): void
}

export type RendererComponent = Component<RendererProps, RendererExports>

export interface RendererCaps {
  /** Canvas-Label-Layer vorhanden → der labels-Toggle wirkt; sonst disabled. */
  labels: boolean
  /** Renderer bringt ein eigenes Layout mit → der FA2-Worker pausiert. */
  ownsLayout: boolean
  /** 3D-Perspektive verfügbar (deck: umschaltbar, three: immer an). */
  threeD: boolean
}

export interface RendererEntry {
  /** Kurzname für die Meta-Row (UI-Sprache der Seite ist englisch). */
  label: string
  caps: RendererCaps
  load: () => Promise<{ default: RendererComponent }>
}

// Die Casts sind nötig, weil Svelte-Komponenten ihre Exports strukturell,
// aber nicht nominal als RendererExports tragen — der Contract wird durch
// die Props-/Export-Signaturen der Komponenten selbst geprüft (svelte-check).
export const RENDERERS: Record<RendererId, RendererEntry> = {
  sigma: {
    label: 'sigma',
    caps: { labels: true, ownsLayout: false, threeD: false },
    load: () => import('../../routes/graph/GraphView.svelte') as Promise<{ default: RendererComponent }>,
  },
  cosmos: {
    label: 'cosmos',
    caps: { labels: false, ownsLayout: true, threeD: false },
    load: () => import('../../routes/graph/renderers/CosmosView.svelte') as Promise<{ default: RendererComponent }>,
  },
  deck: {
    label: 'deck',
    caps: { labels: false, ownsLayout: false, threeD: true },
    load: () => import('../../routes/graph/renderers/DeckView.svelte') as Promise<{ default: RendererComponent }>,
  },
  three: {
    label: 'three',
    caps: { labels: false, ownsLayout: false, threeD: true },
    load: () => import('../../routes/graph/renderers/ThreeView.svelte') as Promise<{ default: RendererComponent }>,
  },
}

export const RENDERER_IDS = Object.keys(RENDERERS) as RendererId[]

const PREF_KEY = 'ctx-graph-renderer'

/** Persistierte Renderer-Wahl; unbekannte/fehlende Werte → sigma (Default,
 *  hält die e2e-visual-Baselines stabil). SSR-/Testumgebungen ohne
 *  localStorage fallen still auf den Default. */
export function loadRendererPref(): RendererId {
  try {
    const raw = localStorage.getItem(PREF_KEY)
    if (raw !== null && raw in RENDERERS) return raw as RendererId
  } catch {
    /* localStorage nicht verfügbar → Default */
  }
  return 'sigma'
}

export function saveRendererPref(id: RendererId): void {
  try {
    localStorage.setItem(PREF_KEY, id)
  } catch {
    /* still ok — die Wahl gilt dann nur für den Seitenbesuch */
  }
}
