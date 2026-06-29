import { mount } from 'svelte'
import './app.css'
import App from './App.svelte'
import { themeController } from './lib/theme/theme.svelte'

// Re-sync the runes store with the data-theme the render-blocking boot script
// (public/theme-boot.js) already painted, and attach the OS-theme listener.
// Runs before mount so the store is correct on the first render; No-FOUC stays
// owned by the boot script — this only mirrors the already-painted state.
themeController.init()

const app = mount(App, {
  target: document.getElementById('app')!,
})

export default app
