/// <reference types="vitest/config" />
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// The API cookie is scoped to /api and the CSRF rotation endpoint compares Origin with Host, so the browser must
// talk to one origin. In development Vite forwards /api to the Go API. The target is the IPv6 loopback because on
// the development machine another server can hold 127.0.0.1:8080. changeOrigin stays false on purpose: it would
// rewrite Host and break the same-origin check. No CORS is involved.
const API_TARGET = 'http://[::1]:8080'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': { target: API_TARGET, changeOrigin: false },
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
  },
})
