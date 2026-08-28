import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  // Firefox does not support module workers; the density renderer runs in a
  // worker, so worker code must be bundled classic-style (DESIGN.md §11).
  worker: { format: 'iife' },
  server: {
    proxy: { '/api': 'http://127.0.0.1:8080' },
  },
})
