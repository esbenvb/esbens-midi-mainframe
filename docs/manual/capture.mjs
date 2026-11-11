// Takes the manual's screenshots of the web interface with headless Chrome (over the
// DevTools protocol, no npm packages) and renders manual.html to a PDF.
//
//   node capture.mjs shots  BASE_URL  OUT_DIR     screenshots of a running emmd (simulated devices)
//   node capture.mjs pdf    HTML_FILE OUT_PDF "footer text"
//
// build.sh runs both. Needs Node 22 (global WebSocket and fetch) and Google Chrome.
import { spawn } from 'node:child_process';
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, existsSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

const CHROME = process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function launch() {
  if (!existsSync(CHROME)) throw new Error(`Chrome not found at ${CHROME} (set CHROME=...)`);
  const dir = mkdtempSync(join(tmpdir(), 'emm-manual-'));
  const proc = spawn(CHROME, ['--headless=new', '--disable-gpu', '--hide-scrollbars', '--no-first-run', '--remote-debugging-port=0',
    `--user-data-dir=${dir}`, 'about:blank'], { stdio: 'ignore' });
  let port = 0;
  for (let i = 0; i < 100 && !port; i++) {
    await sleep(100);
    try { port = Number(readFileSync(join(dir, 'DevToolsActivePort'), 'utf8').split('\n')[0]); } catch (_) { /* not yet */ }
  }
  if (!port) { proc.kill(); throw new Error('Chrome did not start'); }
  const targets = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
  const page = targets.find((t) => t.type === 'page');
  const ws = new WebSocket(page.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  let id = 0;
  const waiting = new Map();
  ws.onmessage = (m) => {
    const d = JSON.parse(m.data);
    if (d.id && waiting.has(d.id)) { const { res, rej } = waiting.get(d.id); waiting.delete(d.id); d.error ? rej(new Error(d.error.message)) : res(d.result); }
  };
  const send = (method, params = {}) => new Promise((res, rej) => { const i = ++id; waiting.set(i, { res, rej }); ws.send(JSON.stringify({ id: i, method, params })); });
  const close = () => { try { ws.close(); } catch (_) {} proc.kill(); try { rmSync(dir, { recursive: true, force: true }); } catch (_) {} };
  return { send, close };
}

async function shots(base, out) {
  mkdirSync(out, { recursive: true });
  const c = await launch();
  try {
    const { send } = c;
    const view = async (w, h) => send('Emulation.setDeviceMetricsOverride', { width: w, height: h, deviceScaleFactor: 1.5, mobile: false });
    const ev = (expression) => send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
    await send('Page.enable');
    await send('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-color-scheme', value: 'light' }] });
    await view(1100, 720);
    const snap = async (name, clip) => {
      const r = await send('Page.captureScreenshot', { format: 'png', ...(clip ? { clip: { ...clip, scale: 1 } } : {}) });
      writeFileSync(join(out, name + '.png'), Buffer.from(r.data, 'base64'));
      console.log('  ' + name + '.png');
    };
    let n = 0;
    // A query string makes every visit a fresh load (a hash change alone keeps toasts and popovers).
    const go = async (hash, wait = 1500) => { await send('Page.navigate', { url: `${base}/?r=${++n}${hash}` }); await sleep(wait); };
    const api = async (method, path, body) => {
      const r = await fetch(base + path, { method, headers: { 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined });
      return r.status === 204 ? null : r.json().catch(() => null);
    };

    // A routing worth looking at: the keyboard plays the synth (one octave up, no clock) and the sampler.
    const state = await api('GET', '/api/state');
    const sim = (name) => (state.ports || []).find((p) => p.kind === 'sim' && p.name === name);
    const kb = sim('Sim Keyboard'), synth = sim('Sim Synth'), sampler = sim('Sim Sampler'), drums = sim('Sim Drum Machine');
    if (kb && synth && sampler && drums) {
      await api('PUT', '/api/connections', { from: kb.id, to: synth.id, filter: { block: ['clock'], transpose: 12 } });
      await api('PUT', '/api/connections', { from: kb.id, to: sampler.id });
      await api('PUT', '/api/connections', { from: drums.id, to: sampler.id });
    }

    await view(1100, 800);
    await go('#matrix', 2500);
    await snap('matrix');

    // The filter editor: right-click the keyboard -> synth cell.
    if (kb && synth) {
      await ev(`(() => { const c = document.querySelector('[data-from="${kb.id}"][data-to="${synth.id}"]'); if (c) c.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 300, clientY: 300 })); })()`);
      await sleep(900);
      await snap('filter-editor', { x: 105, y: 0, width: 455, height: 680 });
    }

    // Presets: save one first so the list is not empty.
    await api('POST', '/api/presets', { name: 'Studio jam', replace: true });
    await go('#matrix', 2000);
    await ev(`[...document.querySelectorAll('button')].find((b) => /Presets/.test(b.textContent))?.click()`);
    await sleep(600);
    await snap('presets', { x: 630, y: 172, width: 480, height: 262 });

    await go('#devices', 2000); await snap('devices');
    await view(1100, 720);
    await go('#monitor', 2500); await snap('monitor');
    await go('#network', 3500); await view(1100, 940); await snap('network');
    await view(1100, 900);
    // Only the settings and backup cards: the device card shows the development machine.
    await go('#system', 2500); await snap('system', { x: 552, y: 128, width: 548, height: 640 });
  } finally { c.close(); }
}

async function pdf(htmlFile, outPdf, footer) {
  const c = await launch();
  try {
    await c.send('Page.enable');
    await c.send('Page.navigate', { url: pathToFileURL(htmlFile).href });
    await sleep(2500);
    const esc = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;');
    const r = await c.send('Page.printToPDF', {
      printBackground: true, preferCSSPageSize: true, displayHeaderFooter: true,
      headerTemplate: '<span></span>',
      footerTemplate: `<div style="width:100%;font-size:8px;color:#777;padding:0 18mm;display:flex;justify-content:space-between;font-family:Helvetica,Arial,sans-serif"><span>${esc(footer)}</span><span><span class="pageNumber"></span> / <span class="totalPages"></span></span></div>`,
    });
    writeFileSync(outPdf, Buffer.from(r.data, 'base64'));
    console.log('  ' + outPdf);
  } finally { c.close(); }
}

const [cmd, a, b, d] = process.argv.slice(2);
if (cmd === 'shots') await shots(a, b);
else if (cmd === 'pdf') await pdf(a, b, d || '');
else { console.error('usage: capture.mjs shots BASE OUT | pdf HTML OUT_PDF FOOTER'); process.exit(2); }
