/*
 * Hermes Desktop head of the settings page: takes React and the UI kit from the host, hands them to the
 * shared core (createPolicyUI, defined further down in the generated bundle) and registers one entry under
 * Settings → Plugins with its sub-pages. Hermes hangs its own config_schema form under the same entry as
 * "Agent settings".
 *
 * Namespace imports on purpose: a component that an older host does not export reads as undefined here and
 * the core falls back to a plain element, instead of the whole file failing to link.
 */
import * as SDK from '@hermes/plugin-sdk'
import * as React from 'react'

const PLUGIN_ID = 'ctx_discord_policy'
const KIT_NAMES = ['ListRow', 'ToggleRow', 'SegmentedControl', 'Button', 'Badge', 'Checkbox', 'Switch', 'Input',
  'Textarea', 'EmptyState', 'StatusDot']

function hostKit() {
  const kit = {}
  KIT_NAMES.forEach(function (name) {
    const component = SDK[name]
    if (typeof component === 'function' || (component && typeof component === 'object')) kit[name] = component
  })
  return kit
}

function settingsHref(page) {
  const sub = page && page !== 'overview' ? page : undefined
  if (typeof SDK.pluginSettingsHref === 'function') return SDK.pluginSettingsHref(PLUGIN_ID, sub)
  return '/settings?tab=plugins&plugin=' + PLUGIN_ID + (sub ? '&ppage=' + sub : '')
}

export default {
  id: PLUGIN_ID,
  name: 'ctx_discord_policy',
  register(ctx) {
    const ui = createPolicyUI({
      React: React,
      kit: hostKit(),
      rest: function (path, opts) { return ctx.rest(path, opts) },
      storage: {
        get: function (key, fallback) { return ctx.storage.get(key, fallback) },
        set: function (key, value) { ctx.storage.set(key, value) },
        remove: function (key) { ctx.storage.remove(key) }
      },
      nav: {
        go: function (page) {
          if (SDK.host && typeof SDK.host.navigate === 'function') SDK.host.navigate(settingsHref(page))
        }
      },
      timers: {
        setTimeout: function (fn, ms) { return ctx.setTimeout(fn, ms) },
        setInterval: function (fn, ms) { return ctx.setInterval(fn, ms) },
        addEventListener: function (target, type, fn, options) { return ctx.addEventListener(target, type, fn, options) }
      }
    })
    const page = {
      id: 'settings',
      title: 'ctx_discord_policy',
      icon: 'shield',
      order: 0,
      render: function () { return React.createElement(ui.Overview) },
      children: ui.pages.map(function (p) {
        return { id: p.id, title: p.title, render: function () { return React.createElement(p.component) } }
      })
    }
    if (typeof ctx.registerSettingsPage === 'function') {
      ctx.registerSettingsPage(page)
    } else {
      ctx.register({
        area: SDK.SETTINGS_PLUGINS_AREA || 'settings.plugins',
        id: page.id,
        title: page.title,
        order: page.order,
        render: page.render,
        data: { icon: page.icon, children: page.children }
      })
    }
    if (typeof ctx.onDispose === 'function') ctx.onDispose(function () { ui.dispose() })
  }
}
