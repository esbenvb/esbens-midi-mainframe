// First-run / captive-portal setup flow.
//
// Written for captive-portal mini-browsers (iOS CNA, Android): no
// localStorage, no popups or target=_blank, no EventSource (it polls), big
// touch targets, plain inline views instead of dialogs.

import { h, clear, replace } from './dom.js';
import { icon, signalBars } from './icons.js';
import { get, post, LONG_TIMEOUT } from './api.js';
import { spinner, notice } from './ui.js';
import { isOpenNet, wifiPasswordProblem, forcedScan } from './network.js';

export function mountSetup(root, net) {
  let data = net || null;
  let nets = (net && net.networks) || [];
  let view = 'choose'; // choose | confirmScan | password | connecting | done
  let sel = null;      // {ssid, security} of the chosen network
  let error = (net && net.status && net.status.last_error) || '';
  let scanning = false;
  let scanWarn = false; // a forced scan in setup mode is running: the setup network is down for a few seconds
  let stale = false;
  let closed = false;

  // attempt tracking
  let timer = 0, fails = 0, started = 0, sawConnecting = false, prevErr = '', attemptSsid = '';

  const status = () => (data && data.status) || {};
  const hostname = () => status().hostname || 'emm';
  const dashboardUrl = () => `http://${hostname()}.local/`;
  // Unknown status (the first request failed): this page is mostly opened from the setup network, so assume it.
  const isSetupMode = () => !!status().setup_mode || status().mode === 'ap' || !status().mode;
  const apName = () => status().ap_ssid || 'EMM-Setup';

  function frame(...body) {
    return h('div', { class: 'setup' },
      h('div', { class: 'setup-brand' },
        h('span', { class: 'mark', 'aria-hidden': 'true' }, 'EMM'),
        h('div', {}, h('h1', {}, 'Welcome to your MIDI Mainframe'), h('p', { class: 'muted' }, 'Esben’s MIDI Mainframe · first-time setup'))),
      ...body);
  }

  function chooseView() {
    const st = status();
    const online = st.mode && st.mode !== 'ap' && !st.setup_mode;
    const apDown = scanning && scanWarn;
    const list = nets.slice().sort((a, b) => (b.signal || 0) - (a.signal || 0)).map((n) => h('li', {},
      h('button', { type: 'button', class: 'net-item big', disabled: apDown, onclick: () => pick(n) },
        signalBars(n.signal),
        h('span', { class: 'net-name' }, n.ssid || '(hidden network)'),
        isOpenNet(n) ? h('span', { class: 'muted small' }, 'open') : icon('lock', 16))));
    return frame(
      online ? notice('info', icon('globe', 16), h('span', {}, 'This device is already online', st.ip ? ` at ${st.ip}` : '', '. ', h('a', { href: '/' }, 'Open the dashboard'), ' or change network below.')) : null,
      h('p', {}, 'Choose your WiFi network. The Mainframe will join it, and then you can reach it from any browser on that network at ', h('strong', { class: 'mono' }, `${hostname()}.local`), '.'),
      error ? notice('error', icon('alert', 16), h('span', {}, error)) : null,
      stale ? notice('info', icon('alert', 16), h('span', {}, 'This list was taken just before the setup network started, so it may be a little old. Rescan for a fresh one.')) : null,
      apDown ? notice('warn', icon('alert', 16), h('span', {}, `The setup network “${apName()}” is switched off for a few seconds while scanning. Your phone may disconnect: that is expected, and it should reconnect by itself. If it doesn’t, join “${apName()}” again in your WiFi settings. This page carries on when the device is back.`)) : null,
      nets.length ? h('ul', { class: 'nets' }, list) : h('p', { class: 'muted' }, scanning ? 'Looking for networks…' : 'No networks found yet. Try Rescan.'),
      h('div', { class: 'btn-stack' },
        h('button', { type: 'button', class: 'btn big', disabled: scanning, onclick: askRescan }, scanning ? spinner() : icon('refresh', 18), scanning ? 'Scanning…' : 'Rescan'),
        h('button', { type: 'button', class: 'btn big', disabled: apDown, onclick: () => pick({ ssid: '', security: '', hidden: true }) }, 'Other network…')),
      h('p', { class: 'muted small' }, 'Prefer a cable? Plug Ethernet in and open ', h('span', { class: 'mono' }, `${hostname()}.local`), ' from a computer on the same network.'));
  }

  // A forced scan takes the setup network down for a few seconds (the phone
  // disconnects), so ask first. An inline view rather than a dialog: see the header.
  function confirmScanView() {
    const ap = apName();
    return frame(
      h('h2', {}, 'Rescan WiFi networks?'),
      h('p', {}, 'To scan, the setup network ', h('strong', {}, ap), ' is switched off for a few seconds. Your phone will lose its connection to it and should reconnect by itself.'),
      h('p', { class: 'muted' }, 'If it doesn’t, join ', h('strong', {}, ap), ' again in your phone’s WiFi settings. This page then shows the fresh list.'),
      h('div', { class: 'btn-stack' },
        h('button', { type: 'button', class: 'btn btn-primary big', onclick: () => rescan(true) }, 'Rescan now'),
        h('button', { type: 'button', class: 'btn big', onclick: () => { view = 'choose'; render(); } }, 'Cancel')));
  }

  function passwordView() {
    const open = isOpenNet(sel) && !sel.hidden;
    const ssid = h('input', { id: 'su-ssid', type: 'text', value: sel.ssid, readOnly: !sel.hidden, maxlength: '32', autocapitalize: 'none', autocorrect: 'off', spellcheck: 'false', autocomplete: 'off' });
    const pw = h('input', { id: 'su-pw', type: 'password', maxlength: '64', autocapitalize: 'none', autocorrect: 'off', spellcheck: 'false', autocomplete: 'off', 'aria-describedby': 'su-hint' });
    const showCb = h('input', { type: 'checkbox', id: 'su-show', onchange: () => { pw.type = showCb.checked ? 'text' : 'password'; } });
    const err = h('p', { class: 'form-error', role: 'alert' }, error);
    const form = h('form', { onsubmit: (e) => {
      e.preventDefault();
      // A scanned network is joined under exactly the name the scan gave (an SSID may
      // begin or end with a space); a typed one under exactly what was typed.
      const s = sel.hidden ? ssid.value : sel.ssid;
      if (!s.trim()) { err.textContent = 'Enter the network name.'; ssid.focus(); return; }
      // An empty password is fine for an open network and for a typed one (nothing is known
      // about it), but not for a network the scan shows as secured: that join cannot work,
      // and from the setup network it would take the access point down for nothing.
      const problem = wifiPasswordProblem(pw.value, { secured: !sel.hidden && !isOpenNet(sel) });
      if (problem) { err.textContent = problem; pw.focus(); return; }
      join(s, pw.value);
    } },
      sel.hidden ? h('div', { class: 'field' }, h('label', { for: 'su-ssid' }, 'Network name'), ssid) : h('h2', { class: 'setup-ssid' }, icon('wifi', 20), ' ', sel.ssid),
      h('div', { class: 'field' },
        h('label', { for: 'su-pw' }, open ? 'Password (this network is open)' : 'WiFi password'),
        pw,
        h('label', { class: 'check', for: 'su-show' }, showCb, ' Show password'),
        h('p', { id: 'su-hint', class: 'muted small' }, sel.hidden ? 'Leave the password empty for an open network.' : '')),
      err,
      h('div', { class: 'btn-stack' },
        h('button', { type: 'submit', class: 'btn btn-primary big' }, 'Connect'),
        h('button', { type: 'button', class: 'btn big', onclick: () => { error = ''; view = 'choose'; render(); } }, 'Back')));
    return frame(form);
  }

  function connectingView() {
    const lost = fails >= 3;
    return frame(
      h('div', { class: 'connecting' }, spinner(), h('h2', {}, `Connecting to ${attemptSsid}…`)),
      h('ol', { class: 'steps' },
        h('li', {}, 'In a moment your phone will leave the ', h('strong', {}, status().ap_ssid || 'EMM-Setup'), ' network. That is expected.'),
        h('li', {}, 'Make sure your phone is on ', h('strong', {}, attemptSsid), ' (it usually rejoins by itself).'),
        h('li', {}, 'Then open ', h('strong', { class: 'mono' }, `http://${hostname()}.local`), ' in your browser.')),
      lost ? notice('info', icon('alert', 16), h('span', {}, 'We have lost contact with the device, which normally means it joined your WiFi. If the setup network comes back instead, reconnect to it and try again — the device will tell you what went wrong.')) : null,
      h('div', { class: 'btn-stack' },
        h('button', { type: 'button', class: 'btn big', onclick: () => { clearTimeout(timer); error = ''; view = 'choose'; render(); } }, 'Cancel and choose again')));
  }

  function doneView() {
    const st = status();
    return frame(
      notice('ok', icon('check', 16), h('span', {}, `Connected${st.ssid ? ' to ' + st.ssid : ''}${st.ip ? ' · ' + st.ip : ''}`)),
      h('p', {}, 'The setup network is going away. Join your normal WiFi on this phone or computer, then open:'),
      h('p', { class: 'big-url mono' }, h('a', { href: dashboardUrl() }, dashboardUrl())),
      st.ip ? h('p', { class: 'muted small' }, 'If that name does not work (some Android and Windows setups), use ', h('a', { href: `http://${st.ip}/` }, `http://${st.ip}/`), ' instead.') : null);
  }

  function render() {
    const active = document.activeElement;
    const keep = active && active.id && root.contains(active) ? active.id : null;
    clear(root);
    root.append(view === 'choose' ? chooseView() : view === 'confirmScan' ? confirmScanView() : view === 'password' ? passwordView() : view === 'connecting' ? connectingView() : doneView());
    if (keep) { const el = root.querySelector('#' + keep); if (el) el.focus({ preventScroll: true }); }
    else if (view === 'password') { const el = root.querySelector('#su-pw'); if (el && !sel.hidden) el.focus({ preventScroll: true }); }
  }

  function pick(n) { sel = n; error = ''; view = 'password'; render(); }

  // The Rescan button asks first in setup mode (the forced scan drops the setup network).
  function askRescan() {
    if (isSetupMode()) { view = 'confirmScan'; render(); } else rescan(true);
  }

  // forced: an explicit tap on Rescan (?force=1). The automatic scan when the page
  // opens is not forced: it must not take the setup network down just for looking.
  async function rescan(forced) {
    if (scanning) return;
    const setup = isSetupMode();
    scanning = true; scanWarn = !!forced && setup;
    view = 'choose'; render();
    try {
      const r = forced ? await forcedScan({ setup, alive: () => !closed }) : await post('/api/network/scan');
      if (closed) return;
      if (r) {
        nets = r.networks || nets;
        stale = !!r.stale;
        if (r.data) data = r.data;
      }
    } catch (e) { error = `Scan failed: ${e.message}`; }
    scanning = false; scanWarn = false;
    if (view === 'choose') render(); // not over the password form the user may be typing in
  }

  async function join(ssid, password) {
    prevErr = status().last_error || '';
    sawConnecting = false; fails = 0; started = Date.now(); attemptSsid = ssid; error = '';
    view = 'connecting'; render();
    try {
      // From the setup network the device answers at once (then leaves it). Already online, the
      // join is synchronous and can take up to a minute, so wait for the answer.
      await post('/api/network/wifi', { ssid, password }, { timeout: isSetupMode() ? 10000 : LONG_TIMEOUT });
    } catch (e) {
      if (e.status) { view = 'password'; error = e.message; render(); return; }
      // status 0 = lost contact: the device most likely already left the setup network
    }
    clearTimeout(timer);
    timer = setTimeout(poll, 1500);
  }

  async function poll() {
    if (view !== 'connecting') return;
    try {
      const d = await get('/api/network', { timeout: 4000 });
      fails = 0;
      data = d;
      const st = d.status || {};
      const busy = st.mode === 'connecting' || st.joining === true;
      if (busy) sawConnecting = true;
      const failedNow = st.setup_mode && !busy && st.last_error &&
        (st.last_error !== prevErr || sawConnecting || Date.now() - started > 25000);
      if (failedNow) {
        error = st.last_error.includes(attemptSsid) ? st.last_error : `Couldn’t join “${attemptSsid}”: ${st.last_error}`;
        view = 'password';
        sel = nets.find((n) => n.ssid === attemptSsid) || { ssid: attemptSsid, security: 'WPA2', hidden: !nets.some((n) => n.ssid === attemptSsid) };
        render();
        return;
      }
      if (!st.setup_mode && !busy && st.mode !== 'ap') { view = 'done'; render(); return; }
    } catch (_) {
      fails++;
      if (fails === 3) render();
    }
    timer = setTimeout(poll, 2000);
  }

  render();
  // Refresh the network list once if the page was opened without one.
  if (!nets.length) rescan(false);
  return () => { closed = true; clearTimeout(timer); };
}
