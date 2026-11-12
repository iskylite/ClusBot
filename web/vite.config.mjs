import {defineConfig} from 'vite'

const frontend = new URL('http://127.0.0.1:5173')
const backend = 'http://127.0.0.1:8081'

export default defineConfig({
  server: {
    host: frontend.hostname,
    port: Number(frontend.port),
    strictPort: true,
    proxy: {
      '/api': {
        target: backend,
        changeOrigin: true,
        configure(proxy) {
          proxy.on('proxyReq', (outgoing, incoming) => {
            if (incoming.headers.origin === frontend.origin) {
              outgoing.setHeader('Origin', backend)
            }
          })
        },
      },
    },
  },
})
