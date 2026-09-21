import { fileURLToPath, URL } from 'node:url'

import tailwindcss from '@tailwindcss/vite'
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vitest/config'

export default defineConfig({
  plugins: [vue(), tailwindcss()],

  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },

  build: {
    // Matches web/embed.go's //go:embed directive, which is what carries the
    // built application into the Go binary.
    outDir: 'dist',
    // Wiping first is what stops stale hashed chunks from accumulating. It also
    // removes the tracked .gitkeep that //go:embed depends on, which
    // scripts/ensure-gitkeep.mjs puts back as the last step of `pnpm build`.
    emptyOutDir: true,
    sourcemap: false,
  },

  server: {
    port: 5173,
    // Development only. In a release the same binary serves the API and these
    // assets, so there is nothing to proxy and no CORS to configure.
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: true,
        // Server-sent events must not be buffered by the dev proxy, or the
        // pipeline progress display never updates while looking connected.
        ws: false,
        configure: (proxy) => {
          proxy.on('proxyRes', (proxyRes) => {
            if (proxyRes.headers['content-type']?.includes('text/event-stream')) {
              proxyRes.headers['x-accel-buffering'] = 'no'
            }
          })
        },
      },
    },
  },

  test: {
    environment: 'jsdom',
    globals: true,
    include: ['src/**/*.spec.ts'],
  },
})
