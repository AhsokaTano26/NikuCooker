import { createPinia } from 'pinia'
import { createApp } from 'vue'

import App from './App.vue'
import { router } from './router'
import './style.css'

/**
 * Reloads the page when a lazily-loaded route cannot be fetched.
 *
 * Every route in this application is a separate file whose name contains a
 * hash of its contents. A browser that is already running one build — a tab
 * left open, or a page loaded just before an upgrade — holds the previous
 * build's file names, and those files are gone the moment the binary is
 * replaced. Clicking a route it has not visited yet asks for a file that no
 * longer exists.
 *
 * Nothing about that failure is visible: the router's navigation is what
 * rejects, and a rejected navigation leaves the previous view on screen. The
 * user clicks, nothing happens, and there is no error to read. Reloading
 * fetches the current document and the current file names, which is the only
 * thing that resolves it.
 *
 * Once per tab. A reload that failed to help would otherwise loop forever, and
 * looping is worse than the blank click — a browser stuck reloading cannot even
 * be used to read the logs about it.
 */
const RELOADED = 'nikucooker.reloaded-for-a-new-build'

function reloadForNewBuild(): void {
  try {
    if (sessionStorage.getItem(RELOADED) === '1') return
    sessionStorage.setItem(RELOADED, '1')
  } catch {
    // Storage can be unavailable — a private window, a blocked partition. The
    // reload is still worth attempting; it just cannot be limited to one.
  }
  window.location.reload()
}

// Vite's own signal, emitted when a module preload fails. It exists precisely
// so an application can recover from this, rather than watching imports reject
// with a message about HTML arriving where JavaScript was expected.
window.addEventListener('vite:preloadError', reloadForNewBuild)

// The same failure reaching the router instead: a route's chunk that could not
// be imported is reported here, and it is indistinguishable from the case
// above as far as the user is concerned.
router.onError((error) => {
  if (/dynamically imported module|Importing a module script failed|preload/i.test(String(error))) {
    reloadForNewBuild()
  }
})

// Cleared once the application is running, so that a later upgrade in the same
// tab gets its own reload rather than being blocked by this one's marker.
router.isReady().then(() => {
  try {
    sessionStorage.removeItem(RELOADED)
  } catch {
    // Nothing to do; the marker is an optimisation, not a requirement.
  }
})

createApp(App).use(createPinia()).use(router).mount('#app')
