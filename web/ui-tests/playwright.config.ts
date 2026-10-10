import { defineConfig } from '@playwright/test';
const port = 5210;
export default defineConfig({
  testDir: '.', testMatch: '*.spec.ts', workers: 1, retries: 0, timeout: 30000,
  reporter: 'list', outputDir: '../test-results/ui',
  use: { baseURL: `http://127.0.0.1:${port}`, locale: 'zh-CN', timezoneId: 'Asia/Shanghai',
    launchOptions: process.env.E2E_BROWSER_EXECUTABLE_PATH ? { executablePath: process.env.E2E_BROWSER_EXECUTABLE_PATH } : {},
    screenshot: 'only-on-failure' },
  projects: [{ name: 'desktop', use: { viewport: {width:1440,height:1000} } }, { name: 'mobile', use: {viewport:{width:390,height:844} } }],
  webServer: { command: `npm run dev -- --port ${port} --strictPort`, url: `http://127.0.0.1:${port}/login`, reuseExistingServer: false, timeout:30000 },
});
