// @live — real login end-to-end (design 06 §4.7 probe 1, wave PV10). The path
// the mock tier structurally bypasses (fixtures write sessionStorage directly):
// here a REAL key hits the real /api/whoami and the shell mounts; a wrong key
// takes the error band and NEVER reaches the shell. Side effect: the genuine
// whoami shape meets the UI, so any capability/field drift vs whoamiFor shows
// up here first (S14).

import { test, expect } from '@playwright/test'
import { readState } from './state'

const state = readState()

test('@live login: real owner key authenticates and mounts the shell', async ({ page }) => {
  await page.goto('/')
  await page.locator('#api-key').fill(state.tenants.b.ownerKey)
  await page.getByRole('button', { name: /sign in/i }).click()
  await expect(page.locator('.shell')).toBeVisible()
})

test('@live login: a wrong key shows the error band and never the shell', async ({ page }) => {
  await page.goto('/')
  // 64 hex chars that no minted key hashes to → real 401 from /api/whoami.
  await page.locator('#api-key').fill('00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff')
  await page.getByRole('button', { name: /sign in/i }).click()
  await expect(page.locator('[role="alert"]')).toBeVisible()
  await expect(page.locator('.shell')).toHaveCount(0)
})
