const { chromium } = require('playwright');

async function runMode(modeId) {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage();
  const logs = [];
  page.on('console', msg => logs.push(`[console.${msg.type()}] ${msg.text()}`));
  page.on('pageerror', err => logs.push(`[pageerror] ${err.message}`));

  try {
    await page.goto('http://127.0.0.1:8090/console/login', { waitUntil: 'domcontentloaded' });
    await page.fill('input[name="username"]', 'admin');
    await page.fill('input[name="password"]', 'admin');
    await page.click('button[type="submit"]');
    await page.waitForURL('**/console', { timeout: 10000 });
    await page.waitForLoadState('networkidle');

    await page.evaluate(() => openPlayer('live/test'));
    await page.waitForSelector('.proto-tab', { timeout: 10000 });
    await page.locator(`.proto-tab[data-proto="${modeId}"]`).click();
    await page.waitForTimeout(10000);

    const result = await page.evaluate(async () => {
      const video = document.getElementById('player-video');
      const status = document.getElementById('player-status')?.textContent || '';
      const url = document.getElementById('player-url')?.textContent || '';
      const pc = window.currentPC || null;
      const stats = [];
      if (pc) {
        const report = await pc.getStats();
        report.forEach(v => {
          if (['inbound-rtp','track','transport','candidate-pair','codec'].includes(v.type)) {
            stats.push(v);
          }
        });
      }
      return {
        status,
        url,
        currentTime: video?.currentTime ?? null,
        paused: video?.paused ?? null,
        readyState: video?.readyState ?? null,
        ended: video?.ended ?? null,
        videoWidth: video?.videoWidth ?? null,
        videoHeight: video?.videoHeight ?? null,
        stats,
      };
    });

    console.log(JSON.stringify({ ok: true, modeId, result, logs }, null, 2));
  } catch (e) {
    console.log(JSON.stringify({ ok: false, modeId, error: e.message, logs }, null, 2));
    process.exitCode = 1;
  } finally {
    await browser.close();
  }
}

(async () => {
  const modeId = process.argv[2] || 'whep-realtime';
  await runMode(modeId);
})();
