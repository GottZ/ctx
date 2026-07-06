// Theme-fester Hover-/Selektions-Renderer (design 02-graph-darkmode §4.4 M4).
//
// Sigmas Default-`drawDiscNodeHover` malt den Hover-/Highlight-Kasten mit
// hartkodiertem `fillStyle = '#FFF'` (node_modules/sigma/dist/
// index-fad77a13.esm.js:666) — auf Dark liegt darauf das Label mit nur 2.59:1.
// Dieser Drawer repliziert die EXAKTE Geometrie des Originals (:655-698), tauscht
// aber die Fläche gegen `palette.hoverBg` (opake Rückfläche, verdeckt die Nodes/
// Edges darunter) und zieht einen 1px-Rahmen in `palette.hoverStroke` — der
// eigentliche Abhebungs-Träger gegen den Canvas (dark 5.17:1, light 5.67:1;
// Gate G4d). Der `#000`-Schatten des Originals bleibt (trägt auf Light, schadet
// auf Dark nicht); der Schatten-State wird nach dem Kasten zurückgesetzt (Spiegel
// von :693-695), sonst erben die Label-Glyphen den Schatten. Das Label zeichnet
// `drawDiscNodeLabel` mit überschriebenem `labelColor` (`palette.hoverLabel`).
//
// Deckt BEIDE Zustände: Maus-Hover UND Fenster-Selektions-Highlight laufen bei
// sigma durch denselben Hover-Renderer (Inventur §4).

import type { Attributes } from 'graphology-types'
import { drawDiscNodeLabel, type NodeHoverDrawingFunction } from 'sigma/rendering'
import type { GraphPalette } from './graph-theme'

/**
 * Baut einen theme-festen Hover-Drawer als Closure über die Palette. Weil sigma-
 * Settings-Funktionen NICHT reaktiv gelesen werden, muss der Aufrufer bei einem
 * Theme-Wechsel eine NEUE Closure setzen (M5-Verdrahtung, setSetting).
 *
 * Generisch über die Graph-Attribut-Typen (N/E/G): sonst würde das konkrete
 * `NodeHoverDrawingFunction<Attributes>` im Sigma-Konstruktor-Setting die
 * Attribut-Generik auf `Attributes` kollabieren und die typisierten Reducer
 * (NodeAttrs/EdgeAttrs) brechen. Der Drawer-Body nutzt N/E/G nicht.
 */
export function makeDrawNodeHover<
  N extends Attributes = Attributes,
  E extends Attributes = Attributes,
  G extends Attributes = Attributes,
>(palette: GraphPalette): NodeHoverDrawingFunction<N, E, G> {
  return (context, data, settings) => {
    const size = settings.labelSize
    const font = settings.labelFont
    const weight = settings.labelWeight
    context.font = `${weight} ${size}px ${font}`

    // Fläche + Schatten (Spiegel :665-670, aber Fläche = hoverBg statt '#FFF').
    context.fillStyle = palette.hoverBg
    context.shadowOffsetX = 0
    context.shadowOffsetY = 0
    context.shadowBlur = 8
    context.shadowColor = '#000'
    const PADDING = 2
    if (typeof data.label === 'string') {
      const textWidth = context.measureText(data.label).width
      const boxWidth = Math.round(textWidth + 5)
      const boxHeight = Math.round(size + 2 * PADDING)
      const radius = Math.max(data.size, size / 2) + PADDING
      const angleRadian = Math.asin(boxHeight / 2 / radius)
      const xDeltaCoord = Math.sqrt(Math.abs(radius ** 2 - (boxHeight / 2) ** 2))
      context.beginPath()
      context.moveTo(data.x + xDeltaCoord, data.y + boxHeight / 2)
      context.lineTo(data.x + radius + boxWidth, data.y + boxHeight / 2)
      context.lineTo(data.x + radius + boxWidth, data.y - boxHeight / 2)
      context.lineTo(data.x + xDeltaCoord, data.y - boxHeight / 2)
      context.arc(data.x, data.y, radius, angleRadian, -angleRadian)
      context.closePath()
      context.fill()
    } else {
      context.beginPath()
      context.arc(data.x, data.y, data.size + PADDING, 0, Math.PI * 2)
      context.closePath()
      context.fill()
    }
    // 1px-Rahmen = theme-feste Abhebung gegen den Canvas (G4d). Auf demselben
    // Pfad wie der Fill; der #000-Schatten trägt ihn auf Light mit.
    context.strokeStyle = palette.hoverStroke
    context.lineWidth = 1
    context.stroke()

    // Schatten-Reset (Pflicht, Spiegel :693-695) — sonst erben Folge-Glyphen ihn.
    context.shadowOffsetX = 0
    context.shadowOffsetY = 0
    context.shadowBlur = 0

    // Label mit theme-fester Farbe (überschreibt settings.labelColor).
    drawDiscNodeLabel(context, data, { ...settings, labelColor: { color: palette.hoverLabel } })
  }
}
