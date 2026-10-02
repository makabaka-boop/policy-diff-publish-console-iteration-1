import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// Build output is embedded into the Go binary via go:embed.
export default defineConfig({
  plugins: [vue()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://localhost:8080',
    },
  },
})
