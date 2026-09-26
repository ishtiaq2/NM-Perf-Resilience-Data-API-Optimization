import { defineConfig } from '@playwright/test';

/**
 * End-to-end tests against a running stack. Point E2E_BASE_URL at:
 *   - the gateway (das-02):          WEB_ROOT=<dist>/browser sh scripts/run-local.sh  ->  http://127.0.0.1:8080
 *   - the Go edge (das-03):          ./das-edge -web <dist>/browser                     ->  http://127.0.0.1:8080
 *   - any backend via scripts/serve.mjs --api <url>                                     ->  http://127.0.0.1:4300
 */
export default defineConfig({
  testDir: 'e2e',
  timeout: 30000,
  use: {
    baseURL: process.env['E2E_BASE_URL'] ?? 'http://127.0.0.1:4300',
    viewport: { width: 1360, height: 860 }
  },
  reporter: 'list'
});
