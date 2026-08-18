import { test, expect } from '@playwright/test';

test('local page loads inside omac sandbox', async ({ page }) => {
  const port = process.env.PORT || '3456';
  await page.goto(`http://127.0.0.1:${port}/`);
  await expect(page.locator('body')).toContainText('omac-playwright-smoke-ok');
});
