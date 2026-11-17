import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react-swc';

export default defineConfig({
  plugins: [react()],
  build: { outDir: '../internal/webui/assets', emptyOutDir: true, license: { fileName: 'third-party-licenses.txt' } },
  server: { proxy: { '/api': 'http://127.0.0.1:8080' } },
});
