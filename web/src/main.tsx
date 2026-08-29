import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App'

// Self-hosted rather than fetched from Google, which the design's markup does.
// smokeng runs on management networks that often have no route off-site: a
// remote font would fail there, and on every other network it would report
// each page view to a third party. Latin only — the UI has no other script.
import '@fontsource/ibm-plex-sans/latin-400.css'
import '@fontsource/ibm-plex-sans/latin-500.css'
import '@fontsource/ibm-plex-sans/latin-600.css'
import '@fontsource/ibm-plex-sans/latin-700.css'
import '@fontsource/ibm-plex-mono/latin-400.css'
import '@fontsource/ibm-plex-mono/latin-500.css'

import './styles.css'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
