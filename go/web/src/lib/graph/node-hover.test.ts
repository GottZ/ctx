// Gate G4b (design 02-graph-darkmode §4.6) — Regressions-Garantie des theme-
// festen Hover-Renderers, beobachtet über einen Recording-2D-Context (das
// TATSÄCHLICH gezeichnete Verhalten, keine Quelltext-Inspektion):
//   Rot-Referenz — sigmas Original `drawDiscNodeHover` malt `fillStyle '#FFF'`
//                  und setzt KEINEN Stroke (der Defekt: Dark-Label darauf 2.59:1).
//   Grün        — `makeDrawNodeHover(palette)` malt `palette.hoverBg` als Fill,
//                  `palette.hoverStroke` als 1px-Stroke, resettet den Schatten
//                  (shadowBlur am Ende 0) und zeichnet das Label in hoverLabel.
//
// Der Shim MUSS vor sigma/rendering stehen (WebGL-Globals, s. sigma-webgl-shim).
import './sigma-webgl-shim'
import { expect, test } from 'vitest'
import { drawDiscNodeHover } from 'sigma/rendering'
import type { Settings } from 'sigma/settings'
import type { NodeDisplayData, PartialButFor } from 'sigma/types'
import { makeDrawNodeHover } from './node-hover'
import type { GraphPalette } from './graph-theme'

// Recording-2D-Context: zeichnet die Farb-/Schatten-/Pfad-Zuweisungen und die
// fill()/stroke()/fillText()-Aufrufe auf (jeweils mit dem AKTUELLEN Style).
function makeRecorder() {
  const rec = {
    fillStyle: '' as string,
    strokeStyle: '' as string,
    lineWidth: 0,
    shadowOffsetX: 0,
    shadowOffsetY: 0,
    shadowBlur: 0,
    shadowColor: '',
    font: '',
    fills: [] as { style: string; shadowBlur: number }[],
    strokes: [] as { style: string; lineWidth: number }[],
    texts: [] as { text: string; style: string }[],
    beginPath() {},
    closePath() {},
    moveTo() {},
    lineTo() {},
    arc() {},
    measureText(t: string) {
      return { width: t.length * 6 } as TextMetrics
    },
    fill() {
      this.fills.push({ style: String(this.fillStyle), shadowBlur: this.shadowBlur })
    },
    stroke() {
      this.strokes.push({ style: String(this.strokeStyle), lineWidth: this.lineWidth })
    },
    fillText(text: string) {
      this.texts.push({ text, style: String(this.fillStyle) })
    },
  }
  return rec
}

type Rec = ReturnType<typeof makeRecorder>
const asCtx = (rec: Rec) => rec as unknown as CanvasRenderingContext2D

const DATA: PartialButFor<NodeDisplayData, 'x' | 'y' | 'size' | 'label' | 'color'> = {
  x: 10,
  y: 20,
  size: 5,
  label: 'Node A',
  color: '#abcabc',
}

const SETTINGS = {
  labelSize: 11,
  labelFont: 'ui-monospace, monospace',
  labelWeight: '400',
  labelColor: { color: '#000000' },
} as unknown as Settings

const PALETTE: GraphPalette = {
  labelColor: '#9aa0bb',
  edgeColor: '#5d5f80',
  edgeStrongColor: '#7d80a8',
  edgeStructuralColor: '#3d9478',
  nodeSat: 70,
  nodeLum: 68,
  hoverBg: '#1e1e28',
  hoverStroke: '#7d80a8',
  hoverLabel: '#d8dae5',
}

test('G4b Rot-Referenz: sigmas drawDiscNodeHover malt fillStyle "#FFF" und setzt KEINEN Stroke', () => {
  const rec = makeRecorder()
  drawDiscNodeHover(asCtx(rec), DATA, SETTINGS)
  // Genau ein Fill, hartkodiert weiß — der Defekt dieser Achse.
  expect(rec.fills.map((f) => f.style)).toEqual(['#FFF'])
  // Kein Rahmen → auf Dark verschwimmt der Kasten (kein Abhebungs-Träger).
  expect(rec.strokes).toEqual([])
})

test('G4b Grün: makeDrawNodeHover malt hoverBg als Fill + hoverStroke als 1px-Stroke + resettet Schatten', () => {
  const rec = makeRecorder()
  makeDrawNodeHover(PALETTE)(asCtx(rec), DATA, SETTINGS)
  // Fläche = palette.hoverBg (statt '#FFF'), mit aktivem #000-Schatten (blur 8).
  expect(rec.fills).toEqual([{ style: '#1e1e28', shadowBlur: 8 }])
  // 1px-Rahmen in palette.hoverStroke = der theme-feste Abhebungs-Träger (G4d).
  expect(rec.strokes).toEqual([{ style: '#7d80a8', lineWidth: 1 }])
  // Schatten-Reset am Ende Pflicht (sonst erben Folge-Glyphen ihn).
  expect(rec.shadowBlur).toBe(0)
  // Label in palette.hoverLabel gezeichnet (überschriebenes labelColor).
  expect(rec.texts).toEqual([{ text: 'Node A', style: '#d8dae5' }])
})
