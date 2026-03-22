const { chromium } = require('playwright');

// Usage: node whep_stats_timeline.js [whep-realtime|whep-live] [duration_sec]
const modeId = process.argv[2] || 'whep-realtime';
const durationSec = parseInt(process.argv[3], 10) || 30;

(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage();

  page.on('pageerror', err => console.error('[pageerror]', err.message));

  try {
    // Login
    await page.goto('http://127.0.0.1:8090/console/login', { waitUntil: 'domcontentloaded' });
    await page.fill('input[name="username"]', 'admin');
    await page.fill('input[name="password"]', 'admin');
    await page.click('button[type="submit"]');
    await page.waitForURL('**/console', { timeout: 10000 });
    await page.waitForLoadState('networkidle');

    // Open player and select WHEP tab
    await page.evaluate(() => openPlayer('live/test'));
    await page.waitForSelector('.proto-tab', { timeout: 10000 });
    await page.locator(`.proto-tab[data-proto="${modeId}"]`).click();

    // Wait for WebRTC connection
    await page.waitForFunction(() => {
      return window.currentPC && window.currentPC.iceConnectionState === 'connected';
    }, { timeout: 15000 });

    console.log(`\nCollecting WebRTC stats for ${durationSec}s (mode: ${modeId})...\n`);

    // Collect periodic stats
    const samples = await page.evaluate(async (count) => {
      const results = [];
      for (let i = 0; i < count; i++) {
        await new Promise(r => setTimeout(r, 1000));
        if (!window.currentPC) break;
        const report = await window.currentPC.getStats();
        const snap = { t: Date.now(), video: null, audio: null };
        report.forEach(s => {
          if (s.type !== 'inbound-rtp') return;
          const entry = {
            kind: s.kind,
            packetsReceived: s.packetsReceived || 0,
            packetsLost: s.packetsLost || 0,
            bytesReceived: s.bytesReceived || 0,
            jitter: s.jitter || 0,
            framesDecoded: s.framesDecoded || 0,
            framesDropped: s.framesDropped || 0,
            framesReceived: s.framesReceived || 0,
            keyFramesDecoded: s.keyFramesDecoded || 0,
            jitterBufferDelay: s.jitterBufferDelay || 0,
            jitterBufferEmittedCount: s.jitterBufferEmittedCount || 0,
            freezeCount: s.freezeCount || 0,
            totalFreezesDuration: s.totalFreezesDuration || 0,
            pliCount: s.pliCount || 0,
            firCount: s.firCount || 0,
            nackCount: s.nackCount || 0,
          };
          if (s.kind === 'video') snap.video = entry;
          if (s.kind === 'audio') snap.audio = entry;
        });
        results.push(snap);
      }
      return results;
    }, durationSec);

    if (samples.length === 0) {
      console.error('No stats collected — connection may have failed.');
      process.exitCode = 1;
      return;
    }

    // Print delta table
    const hdr = [
      'Sec', 'FPS(dec)', 'Dropped', 'KeyFr', 'PktRecv', 'PktLost',
      'Bitrate', 'JitBuf(ms)', 'Freeze', 'PLI', 'NACK', 'Jitter(ms)',
      'A.Lost', 'A.Jit(ms)'
    ];
    const colW = [4, 9, 7, 5, 8, 7, 9, 11, 6, 4, 5, 11, 6, 9];
    const pad = (s, w) => String(s).padStart(w);

    console.log(hdr.map((h, i) => pad(h, colW[i])).join(' | '));
    console.log(colW.map(w => '-'.repeat(w)).join('-+-'));

    let prev = samples[0];
    for (let i = 1; i < samples.length; i++) {
      const cur = samples[i];
      const v = cur.video;
      const pv = prev.video;
      const a = cur.audio;
      const pa = prev.audio;

      if (!v || !pv) { prev = cur; continue; }

      const dtSec = (cur.t - prev.t) / 1000;
      const dDecoded = v.framesDecoded - pv.framesDecoded;
      const dDropped = v.framesDropped - pv.framesDropped;
      const dKeyFr = v.keyFramesDecoded - pv.keyFramesDecoded;
      const dPktRecv = v.packetsReceived - pv.packetsReceived;
      const dPktLost = v.packetsLost - pv.packetsLost;
      const dBytes = v.bytesReceived - pv.bytesReceived;
      const bitrateMbps = ((dBytes * 8) / dtSec / 1e6).toFixed(2);
      const dJBDelay = v.jitterBufferDelay - pv.jitterBufferDelay;
      const dJBCount = v.jitterBufferEmittedCount - pv.jitterBufferEmittedCount;
      const jitBufMs = dJBCount > 0 ? ((dJBDelay / dJBCount) * 1000).toFixed(0) : '-';
      const dFreeze = v.freezeCount - pv.freezeCount;
      const dPLI = v.pliCount - pv.pliCount;
      const dNACK = v.nackCount - pv.nackCount;
      const jitterMs = (v.jitter * 1000).toFixed(1);

      const aLost = (a && pa) ? (a.packetsLost - pa.packetsLost) : 0;
      const aJit = a ? (a.jitter * 1000).toFixed(1) : '-';

      const row = [
        i, dDecoded, dDropped, dKeyFr, dPktRecv, dPktLost,
        bitrateMbps, jitBufMs, dFreeze, dPLI, dNACK, jitterMs,
        aLost, aJit
      ];
      const line = row.map((val, ci) => {
        let s = pad(val, colW[ci]);
        // Color red for dropped/lost/freeze
        if ((ci === 2 || ci === 5 || ci === 8 || ci === 12) && Number(val) > 0) {
          s = `\x1b[31m${s}\x1b[0m`;
        }
        // Color yellow for high jitter buffer (>200ms)
        if (ci === 7 && Number(val) > 200) {
          s = `\x1b[33m${s}\x1b[0m`;
        }
        return s;
      });
      console.log(line.join(' | '));
      prev = cur;
    }

    // Summary
    const first = samples[0];
    const last = samples[samples.length - 1];
    if (first.video && last.video) {
      const v0 = first.video;
      const vN = last.video;
      const totalSec = (last.t - first.t) / 1000;
      console.log('\n--- Summary ---');
      console.log(`Duration:       ${totalSec.toFixed(1)}s`);
      console.log(`Frames decoded: ${vN.framesDecoded - v0.framesDecoded}`);
      console.log(`Frames dropped: ${vN.framesDropped - v0.framesDropped}`);
      console.log(`Keyframes:      ${vN.keyFramesDecoded - v0.keyFramesDecoded}`);
      console.log(`Packets lost:   ${vN.packetsLost - v0.packetsLost} / ${vN.packetsReceived - v0.packetsReceived} received`);
      console.log(`Freezes:        ${vN.freezeCount - v0.freezeCount} (${((vN.totalFreezesDuration - v0.totalFreezesDuration) * 1000).toFixed(0)}ms total)`);
      console.log(`PLI sent:       ${vN.pliCount - v0.pliCount}`);
      console.log(`NACK sent:      ${vN.nackCount - v0.nackCount}`);

      const dJBDelay = vN.jitterBufferDelay - v0.jitterBufferDelay;
      const dJBCount = vN.jitterBufferEmittedCount - v0.jitterBufferEmittedCount;
      if (dJBCount > 0) {
        console.log(`Avg JitBuf:     ${((dJBDelay / dJBCount) * 1000).toFixed(0)}ms`);
      }

      const avgFPS = (vN.framesDecoded - v0.framesDecoded) / totalSec;
      console.log(`Avg FPS:        ${avgFPS.toFixed(1)}`);

      const totalBytes = vN.bytesReceived - v0.bytesReceived;
      console.log(`Avg bitrate:    ${((totalBytes * 8) / totalSec / 1e6).toFixed(2)} Mbps`);
    }

  } catch (e) {
    console.error('Error:', e.message);
    process.exitCode = 1;
  } finally {
    await browser.close();
  }
})();
