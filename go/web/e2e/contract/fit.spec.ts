import { expect, test, type Page } from '@playwright/test'
import { expectViewportFit } from './fit'

// Meta-test of the viewport fit itself (#440): synthetic pages with boxes of
// fixed pixel sizes (no text, so no font dependence — runs on the host and in
// the container). Each failing case pins one layer of fit.ts; the passing
// cases pin its structural exemptions, so a widened exemption or a dropped
// edge turns this file red.

test.use({ viewport: { width: 390, height: 844 } })

const BASE = '<style>html,body{margin:0}main{display:block}</style>'

async function page390(page: Page, body: string, css = ''): Promise<void> {
  await page.setContent(`<!doctype html><html><head>${BASE}<style>${css}</style></head><body>${body}</body></html>`)
}

test.describe('viewport fit (meta, #440)', () => {
  test('a page whose boxes fit passes', async ({ page }) => {
    await page390(page, '<main class="content" style="height:200px"><div style="height:20px"></div></main>', '.content{overflow-y:auto}')
    await expectViewportFit(page)
  })

  test('a box 1 px wider than a region that only scrolls down fails as clipped', async ({ page }) => {
    // The document stays 390 wide: the region absorbs the overflow (#439).
    await page390(
      page,
      '<main class="content" style="width:300px;height:200px"><div style="width:301px;height:20px"></div></main>',
      '.content{overflow-y:auto}',
    )
    await expect(expectViewportFit(page)).rejects.toThrow(/clipped: main\.content holds 301 px of content in 300 px/)
  })

  test('a box that starts left of a region that only scrolls down fails as clipped', async ({ page }) => {
    // scrollWidth never counts overflow left of the scroll origin.
    await page390(
      page,
      '<main class="content" style="margin-left:50px;width:300px;height:200px"><div style="margin-left:-8px;width:100px;height:20px"></div></main>',
      '.content{overflow-y:auto}',
    )
    await expect(expectViewportFit(page)).rejects.toThrow(/clipped: main\.content > div starts at x 42, left of the padding edge 50/)
  })

  test('a document taller or wider than the viewport fails', async ({ page }) => {
    await page390(page, '<div style="height:845px"></div>')
    await expect(expectViewportFit(page)).rejects.toThrow(/the document is 1 px taller than the 844 px viewport/)
    await page390(page, '<div style="width:391px;height:20px"></div>')
    await expect(expectViewportFit(page)).rejects.toThrow(/the document is 1 px wider than the 390 px viewport/)
  })

  test('an overflow-x declaration counts only while its @media or @supports condition holds', async ({ page }) => {
    const body = '<div class="x" style="width:300px;height:100px"><div style="width:400px;height:20px"></div></div>'
    await page390(page, body, '.x{overflow-y:auto}@media (min-width: 100px){.x{overflow-x:auto}}')
    await expectViewportFit(page)
    await page390(page, body, '.x{overflow-y:auto}@media (max-width: 100px){.x{overflow-x:auto}}')
    await expect(expectViewportFit(page)).rejects.toThrow(/clipped: div\.x holds 400 px of content in 300 px/)
    await page390(page, body, '.x{overflow-y:auto}@supports not (display: block){.x{overflow-x:auto}}')
    await expect(expectViewportFit(page)).rejects.toThrow(/clipped: div\.x holds 400 px of content in 300 px/)
  })

  test('main.content scrolls sideways only in board mode', async ({ page }) => {
    const css = ".content{overflow-y:auto}.content[data-mode='board']{overflow-x:auto}"
    const body = (mode: string): string =>
      `<main class="content" data-mode="${mode}" style="height:200px"><div style="width:900px;height:20px"></div></main>`
    await page390(page, body('board'), css)
    await expectViewportFit(page)
    await page390(page, body('split'), css)
    await expect(expectViewportFit(page)).rejects.toThrow(/clipped: main\.content\[data-mode='split'\]/)
  })

  test('a frame that declares overflow: hidden may not cut a box off sideways', async ({ page }) => {
    await page390(page, '<div class="frame" style="width:300px;height:100px"><div style="width:301px;height:20px"></div></div>', '.frame{overflow:hidden}')
    await expect(expectViewportFit(page)).rejects.toThrow(/clipped: div\.frame holds 301 px of content in 300 px \(overflow-x hidden/)
  })

  test('a scroller declaration that loses the cascade does not exempt the region', async ({ page }) => {
    // The later overflow: hidden wins; the earlier overflow-x: auto still
    // matches the element but is not in effect.
    await page390(
      page,
      '<div class="frame" style="width:300px;height:100px"><div style="width:400px;height:20px"></div></div>',
      '.frame{overflow-x:auto}.frame{overflow:hidden}',
    )
    await expect(expectViewportFit(page)).rejects.toThrow(/clipped: div\.frame holds 400 px of content in 300 px \(overflow-x hidden/)
  })

  test('a region that declares overflow-x may scroll sideways, its own box may not stick out', async ({ page }) => {
    await page390(page, '<div class="scroll"><div style="width:900px;height:20px"></div></div>', '.scroll{overflow-x:auto}')
    await expectViewportFit(page)
    await page390(page, '<div style="overflow-x:auto"><div style="width:900px;height:20px"></div></div>')
    await expectViewportFit(page)
    await page390(
      page,
      '<div class="scroll" style="width:200px;margin-left:-1px"><div style="width:900px;height:20px"></div></div>',
      '.scroll{overflow-x:auto}',
    )
    await expect(expectViewportFit(page)).rejects.toThrow(/outside: div\.scroll spans x -1\.\.199/)
  })

  test('a box 1 px past the left edge fails, though the document does not grow', async ({ page }) => {
    await page390(page, '<div style="position:relative;left:-1px;width:100px;height:20px"></div>')
    await expect(expectViewportFit(page)).rejects.toThrow(/outside: div spans x -1\.\.99/)
  })

  test('a fixed box past the right edge fails, also inside a clipping region', async ({ page }) => {
    await page390(
      page,
      '<div style="overflow:hidden;width:200px;height:100px"><div style="position:fixed;top:0;right:-1px;width:50px;height:20px"></div></div>',
    )
    await expect(expectViewportFit(page)).rejects.toThrow(/outside: div > div spans x 341\.\.391/)
  })

  test('a modal dialog wider than the viewport fails, also opened inside a scrolling region', async ({ page }) => {
    // The second dialog leaves the UA's position: fixed and sticks out on the
    // left, where the document does not grow: in the top layer the region
    // still does not clip it.
    for (const style of ['width:400px', 'position:absolute;left:-10px;margin:0;width:100px']) {
      await page390(
        page,
        `<main class="content" style="height:200px"><dialog style="${style};height:20px;max-width:none;padding:0;border:0"></dialog></main>`,
        '.content{overflow-y:auto}',
      )
      await page.evaluate(() => document.querySelector('dialog')?.showModal())
      await expect(expectViewportFit(page)).rejects.toThrow(/outside: main\.content > dialog spans x /)
    }
  })

  test('visually hidden labels, ellipsis and form controls clip on purpose and pass', async ({ page }) => {
    await page390(
      page,
      '<span class="sr"><span style="display:inline-block;width:900px;height:1px"></span></span>' +
        '<div class="ell"><span style="display:inline-block;width:900px;height:20px"></span></div>' +
        `<input style="width:100px" value="${'x'.repeat(200)}">`,
      // .ell clips through overflow-y alone (overflow-x computes to auto, nothing
      // declares it), so only the ellipsis exemption lets it pass.
      // .sr sits off-screen like a skip link; only the clip-path exemption lets it pass.
      '.sr{position:absolute;left:-10000px;width:1px;height:1px;overflow:hidden;clip-path:inset(50%)}' +
        '.ell{width:100px;overflow-y:hidden;white-space:nowrap;text-overflow:ellipsis}',
    )
    await expectViewportFit(page)
  })
})
