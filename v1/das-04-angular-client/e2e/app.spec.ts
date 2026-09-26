import { expect, test } from '@playwright/test';

test('dashboard shows every Remote Node and keeps receiving updates', async ({ page }) => {
  await page.goto('/dashboard');
  await expect(page.getByTestId('node-count')).not.toHaveText('0', { timeout: 15000 });
  const transport = await page.getByTestId('telemetry-transport').innerText();
  expect(['WebSocket push', 'HTTP deltas (ETag)', 'HTTP polling (legacy backend)']).toContain(transport);
  const rev = async () => Number((await page.getByTestId('statusline').innerText()).match(/rev (\d+)/)?.[1] ?? -1);
  const r0 = await rev();
  await expect.poll(rev, { timeout: 10000 }).toBeGreaterThan(r0);
  await expect(page.getByTestId('connection')).toContainText('Connected');
});

test('spectrum analyzer receives successive sweeps', async ({ page }) => {
  await page.goto('/spectrum');
  const sweep = page.getByTestId('sweep-id');
  await expect(sweep).toBeVisible({ timeout: 15000 });
  const first = Number((await sweep.innerText()).replace('#', ''));
  await expect.poll(async () => Number((await sweep.innerText()).replace('#', '')), { timeout: 10000 }).toBeGreaterThan(first);
});

test('configuration: a concurrent edit is refused instead of silently overwritten', async ({ page, request }) => {
  const caps = await request.get('/api/capabilities');
  test.skip(caps.status() === 404, 'legacy backend has no optimistic locking');
  await page.goto('/nodes/9');
  await page.getByRole('button', { name: "Simulate a colleague's edit" }).click();
  await page.getByTestId('save-config').click();
  await expect(page.getByTestId('config-notice')).toContainText('someone else changed this node');
});
