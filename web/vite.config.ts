import { defineConfig, loadEnv } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, '.', 'VEDIOAGENT_')
  return {
    plugins: [vue()],
    server: { proxy: { '/v1': env.VEDIOAGENT_API_TARGET || 'http://127.0.0.1:8080' } },
  }
})
