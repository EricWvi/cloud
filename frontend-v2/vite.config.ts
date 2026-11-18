import path from 'node:path'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

// v2-server's address; the end-to-end tests point it at their own instance.
const backend = process.env.V2_SERVER ?? 'http://127.0.0.1:8090'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { '@': path.resolve(import.meta.dirname, './src') },
  },
  server: {
    // v2-server's default address (see v2-server/README.md). Like Cloud's Gateway, it serves the
    // login routes, the API and the health check; nothing else is proxied. The browser's Host is
    // kept (Vite's string shorthand would rewrite it): without V2_PUBLIC_URL, v2-server accepts a
    // cookie-authenticated write only when its Origin names the Host the request arrived at.
    proxy: {
      '/api': { target: backend, changeOrigin: false },
      '/auth': { target: backend, changeOrigin: false },
      '/healthz': { target: backend, changeOrigin: false },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    // settings-css.test.ts minifies this file; vitest empties every other stylesheet.
    css: { include: [/settings\.css/] },
    include: ['src/**/*.test.{ts,tsx}'],
  },
})
