// Network tab: status, WiFi networks, Ethernet, hostname.

import { h, clear, replace } from './dom.js';
import { icon, signalBars } from './icons.js';
import { get, post, put, del, ApiError, LONG_TIMEOUT, SCAN_TIMEOUT } from './api.js';
import { S, on } from './state.js';
import { toast, badge, spinner, notice, openModal, confirmDialog, kv, emmTxtBadge, isTextField, captureFocus, restoreFocus } from './ui.js';

export const MODE_TEXT = {
  client: 'Connected to WiFi',
  ap: 'Setup network (access point)',
  ethernet: 'Ethernet only',
  connecting: 'Connecting…',
  offline: 'Offline',
  unmanaged: 'Network not managed by EMM',
};

export const isOpenNet = (n) => !n || !n.security || /^(open|--|none)$/i.test(n.security);

const IPV4 = /^(25[0-5]|2[0-4]\d|1?\d?\d)(\.(25[0-5]|2[0-4]\d|1?\d?\d)){3}$/;
const CIDR = /^(25[0-5]|2[0-4]\d|1?\d?\d)(\.(25[0-5]|2[0-4]\d|1?\d?\d)){3}\/(3[0-2]|[12]?\d)$/;
const HOSTNAME = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

// boot_managed names the setting; emm.txt itself has these keys.
const EMM_KEYS = {
  hostname: 'hostname',
  wifi: 'wifi_ssid / wifi_password',
  wifi_country: 'wifi_country',
  ethernet: 'ethernet',
};

// What the daemon accepts as a WiFi password: empty (open network), 8-63
// characters (it counts bytes), or exactly 64 hex digits (a raw key). Returns
// an explanation, or '' when the password is acceptable. Shared with setup.js.
// `secured`: the scan shows this network as protected, so an empty password can
// only end in a failed join (the daemon cannot tell, it would try it as an open
// network). Typing a hidden network's name leaves it false: nothing is known there.
const byteLen = (s) => (typeof TextEncoder === 'function' ? new TextEncoder().encode(s).length : s.length);
export function wifiPasswordProblem(p, { secured = false } = {}) {
  if (p === '') return secured ? 'This network needs a password.' : '';
  if (/^[0-9a-fA-F]{64}$/.test(p)) return '';
  const n = byteLen(p);
  if (n >= 8 && n <= 63) return '';
  return 'A WiFi password has 8–63 characters, or exactly 64 hex digits. Leave it empty only for an open network.';
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// A forced WiFi scan (the Rescan buttons). With the setup access point up the
// radio has to leave AP mode to scan, so the phone loses its connection for a
// few seconds and the request itself can fail with status 0 (or hang). In setup
// mode we therefore also watch /api/network, and finish as soon as either the
// request answers or the device is back with a list newer than this request.
// alive() returning false abandons the wait (the result is null). Shared with setup.js.
export async function forcedScan({ setup = false, alive = () => true } = {}) {
  const started = Date.now();
  const fromScan = (r) => ({ networks: (r && r.networks) || [], stale: !!(r && r.stale), data: null });
  const posting = post('/api/network/scan?force=1', undefined, { timeout: SCAN_TIMEOUT }).then((r) => ({ r }), (e) => ({ e }));
  if (!setup) {
    const o = await posting;
    if (o.e) throw o.e;
    return fromScan(o.r);
  }
  let finished = false;
  const watching = (async () => {
    const giveUp = started + 90000;
    await sleep(3000);
    while (!finished && alive() && Date.now() < giveUp) {
      try {
        const d = await get('/api/network', { timeout: 4000 });
        // scan_age is seconds since the cached scan; no older than this request = the scan is done.
        const age = d && d.status ? d.status.scan_age : -1;
        if (!finished && age >= 0 && age <= Math.floor((Date.now() - started) / 1000)) return { fresh: d };
      } catch (_) { /* the access point is still down */ }
      if (!finished) await sleep(2000);
    }
    return { gaveUp: true };
  })();
  try {
    let o = await Promise.race([posting, watching]);
    if (o.e && !o.e.status) o = await watching; // connection lost: wait for the device to come back
    if (o.e) throw o.e;
    if (o.fresh) return { networks: o.fresh.networks || [], stale: false, data: o.fresh };
    if (o.gaveUp) {
      if (!alive()) return null;
      throw new ApiError('The device did not come back after the scan. Reconnect to its setup network and try again.', 0);
    }
    return fromScan(o.r);
  } finally {
    finished = true;
  }
}

export function mount(root) {
  const host = h('section', { class: 'network' });
  root.append(host);

  let data = S.network;
  let networks = (data && data.networks) || [];
  let stale = false;
  let scanning = false;
  let scanWarn = false;      // a forced scan in setup mode: the setup network is down for a few seconds
  let joiningSsid = '';      // a join request is in flight (they can take up to a minute)
  let forgetting = false;
  let applyingEth = false;
  let pollTimer = 0;
  let editing = false;       // focus is in a text field of the cards: they are not rebuilt under the cursor
  let pending = false;       // ...so a refresh that arrives meanwhile waits until focus has left them
  let alive = true;          // false once the tab is left: late responses are ignored

  // force: a save has just succeeded. The cards must show the new state even though
  // the field that was submitted (Enter) still has focus; it comes back with focus.
  async function load({ force = false } = {}) {
    try {
      const d = await get('/api/network');
      if (!alive) return;
      data = d;
      S.network = data;
      networks = data.networks || networks;
    } catch (e) {
      if (!alive) return;
      toast(`Couldn't read network status: ${e.message}`, 'error');
    }
    render({ force });
    schedulePoll();
  }
  function schedulePoll() {
    clearTimeout(pollTimer);
    if (!alive) return;
    if (data && data.status && (data.status.mode === 'connecting' || data.status.joining)) pollTimer = setTimeout(load, 2000);
  }

  // Rescan always forces a scan (?force=1); without it the daemon in setup mode
  // just returns the list cached before the access point started. A forced scan
  // in setup mode switches the setup network off for a few seconds, so ask first.
  async function rescan() {
    if (scanning) return;
    const st = (data && data.status) || {};
    const setup = !!st.setup_mode || st.mode === 'ap';
    if (setup) {
      const ap = st.ap_ssid || 'EMM-Setup';
      const ok = await confirmDialog({
        title: 'Rescan WiFi networks?',
        body: `To scan, the setup network “${ap}” is switched off for a few seconds. Your phone or computer will lose its connection to it and should reconnect by itself. If it doesn’t, join “${ap}” again in its WiFi settings. The fresh list appears here as soon as the connection is back.`,
        confirmLabel: 'Rescan',
      });
      if (!ok || !alive) return;
    }
    scanning = true;
    scanWarn = setup;
    render();
    let fresh = false;
    try {
      const r = await forcedScan({ setup, alive: () => alive });
      if (!r) return; // tab left while waiting
      networks = r.networks;
      stale = r.stale;
      if (r.data) { data = r.data; S.network = r.data; }
      fresh = true;
    } catch (e) { if (alive) toast(`Scan failed: ${e.message}`, 'error'); }
    if (!alive) return;
    scanning = false;
    scanWarn = false;
    if (fresh) load(); // refreshes scan_age, so the "list may be out of date" note goes away
    else render();
  }

  const managed = (...keys) => ((data && data.boot_managed) || []).some((k) => keys.includes(k));
  const managedBadge = (...keys) => (managed(...keys) ? emmTxtBadge() : null);
  const fixedText = (key) => `Set by emm.txt (${key}). Edit the file on the SD card to change it.`;

  // ---- cards
  function statusCard() {
    const st = (data && data.status) || {};
    const eth = st.ethernet || {};
    // emm.txt fixes the hostname: changing it would only end in a 409.
    const lockHost = managed('hostname');
    const hostInput = h('input', { type: 'text', id: 'hostname', value: st.hostname || S.hostname, maxlength: '63', autocapitalize: 'none', spellcheck: 'false', disabled: lockHost, 'aria-describedby': 'hn-hint' });
    const hostErr = h('p', { class: 'form-error', role: 'alert' });
    const form = h('form', { class: 'inline-form', onsubmit: async (e) => {
      e.preventDefault();
      if (lockHost) return;
      const v = hostInput.value.trim().toLowerCase();
      if (!HOSTNAME.test(v)) { hostErr.textContent = 'Use letters, digits and hyphens only (no leading/trailing hyphen), up to 63 characters.'; return; }
      hostErr.textContent = '';
      try { await put('/api/network/hostname', { hostname: v }); toast(`Hostname set to ${v}.local`); load({ force: true }); } catch (er) { hostErr.textContent = er.message; }
    } },
      h('label', { for: 'hostname' }, 'Hostname ', managedBadge('hostname')),
      h('div', { class: 'inline-form-row' }, hostInput, h('span', { class: 'suffix mono' }, '.local'), h('button', { id: 'hostname-save', type: 'submit', class: 'btn', disabled: lockHost }, 'Save')),
      h('p', { id: 'hn-hint', class: 'muted small' }, lockHost ? fixedText('hostname') : 'The device is reachable at this name on your network.'),
      hostErr);
    return h('article', { class: 'card' },
      h('header', { class: 'card-head' }, h('h3', {}, icon('globe', 16), ' Status'),
        h('div', { class: 'badges' }, badge(MODE_TEXT[st.mode] || st.mode || 'unknown', st.mode === 'client' || st.mode === 'ethernet' ? 'ok' : st.mode === 'ap' || st.mode === 'connecting' ? 'warn' : ''))),
      st.last_error ? notice('error', icon('alert', 16), h('span', {}, `Last problem: ${st.last_error}`)) : null,
      // Problems found in emm.txt (e.g. an invalid ap_password makes the setup
      // network open). The daemon never includes the offending values.
      ...((st.warnings || []).map((w) => notice('warn', icon('alert', 16), h('span', {}, `emm.txt: ${w}`)))),
      // Online through a connection EMM did not create (e.g. an OS install that was already on WiFi). Older daemons do not send the flag.
      st.adopted ? notice('info', icon('globe', 16), h('span', {}, 'Connected through a network that was set up outside EMM; EMM leaves it alone.')) : null,
      joiningSsid
        ? h('p', { class: 'row-gap', role: 'status' }, spinner(), ` Joining “${joiningSsid}”… this can take up to a minute while the device tries the password.`)
        : st.mode === 'connecting' || st.joining ? h('p', { class: 'row-gap' }, spinner(), ' Working on it… this page updates by itself.') : null,
      kv([
        ['WiFi network', st.ssid || (st.mode === 'ap' ? `${st.ap_ssid || 'EMM-Setup'} (setup network)` : '—')],
        ['WiFi address', st.ip || '—'],
        ['Ethernet', eth.link ? `connected${eth.ip ? ' · ' + eth.ip : ''}` : 'no cable'],
        ['Reachable as', `${st.hostname || S.hostname}.local`],
      ]),
      form);
  }

  function wifiCard() {
    const st = (data && data.status) || {};
    const saved = data && data.saved_ssid;
    // emm.txt fixes the WiFi network: joining or forgetting would only end in a 409.
    const lockWifi = managed('wifi');
    const noJoin = lockWifi || !!joiningSsid || forgetting;
    // The scan list can predate the connection, so also match on the live SSID.
    const list = networks.map((n) => ({ ...n, in_use: !!n.in_use || (st.mode === 'client' && !!st.ssid && n.ssid === st.ssid) }))
      .sort((a, b) => (b.in_use - a.in_use) || (b.signal - a.signal));
    const items = list.map((n) => h('li', {},
      h('button', { type: 'button', class: 'net-item' + (n.in_use ? ' in-use' : ''), disabled: noJoin, dataset: { fk: 'net:' + (n.ssid || '') }, onclick: () => joinDialog(n) },
        signalBars(n.signal),
        h('span', { class: 'net-name' }, n.ssid || '(hidden network)'),
        n.ssid && n.ssid === saved ? badge('saved') : null,
        n.in_use ? badge('connected', 'ok') : null,
        h('span', { class: 'net-sec muted small' }, isOpenNet(n) ? 'open' : n.security, isOpenNet(n) ? null : [' ', icon('lock', 12)]))));
    return h('article', { class: 'card' },
      h('header', { class: 'card-head' }, h('h3', {}, icon('wifi', 16), ' WiFi ', managedBadge('wifi')),
        h('div', { class: 'btn-row' },
          // aria-disabled, not disabled: the button keeps keyboard focus while the scan runs (rescan() ignores the click).
          h('button', { type: 'button', class: 'btn btn-small', 'aria-disabled': scanning ? 'true' : null, dataset: { fk: 'rescan' }, onclick: rescan }, scanning ? spinner() : icon('refresh', 14), scanning ? 'Scanning…' : 'Rescan'))),
      lockWifi
        ? notice('info', icon('lock', 16), h('span', {}, 'The WiFi network is set in ', h('code', {}, 'emm.txt'), ' (', h('code', {}, EMM_KEYS.wifi), ') and re-applied at every boot, so you can’t join or forget networks here. Edit the file on the SD card to change it.'))
        : null,
      scanWarn
        ? notice('warn', icon('alert', 16), h('span', {}, `The setup network “${st.ap_ssid || 'EMM-Setup'}” is switched off for a few seconds while scanning. If your phone or computer disconnected, it should reconnect by itself; if not, join the setup network again in its WiFi settings. This page carries on when the device is back.`))
        : null,
      stale || (st.scan_age > 120 && st.mode === 'ap')
        ? notice('info', icon('alert', 16), h('span', {}, 'This list was captured before the setup network started, so it may be out of date. A rescan briefly interrupts the setup network.'))
        : null,
      items.length ? h('ul', { class: 'nets' }, items) : h('p', { class: 'muted' }, scanning ? 'Scanning…' : 'No networks found. Try Rescan.'),
      h('div', { class: 'btn-row row-gap' },
        h('button', { type: 'button', class: 'btn', disabled: noJoin, dataset: { fk: 'join-hidden' }, onclick: () => joinDialog(null) }, icon('plus', 14), 'Join a hidden network…'),
        saved ? h('button', { type: 'button', class: 'btn btn-danger', disabled: lockWifi, 'aria-disabled': forgetting || joiningSsid ? 'true' : null, dataset: { fk: 'forget' }, onclick: forget },
          forgetting ? spinner() : icon('trash', 14), forgetting ? 'Forgetting…' : `Forget “${saved}”`) : null),
      forgetting ? h('p', { class: 'muted small', role: 'status' }, 'Removing the saved network. This can take up to a minute.') : null);
  }

  async function forget() {
    if (forgetting || joiningSsid) return;
    const saved = data && data.saved_ssid;
    const note = managed('wifi') ? ' Note: emm.txt also contains a WiFi network, which is re-applied at every boot.' : '';
    if (!(await confirmDialog({ title: 'Forget WiFi network?', body: `The saved network “${saved}” will be removed from this device.${note}`, confirmLabel: 'Forget', danger: true }))) return;
    forgetting = true;
    render();
    try { await del('/api/network/wifi', { timeout: LONG_TIMEOUT }); toast('Network forgotten'); } catch (e) { toast(e.message, 'error'); }
    forgetting = false;
    if (alive) load();
  }

  function ethernetCard() {
    const e = (data && data.ethernet) || { mode: 'auto', address: '', gateway: '', dns: [] };
    // emm.txt fixes the Ethernet setup: changing it would only end in a 409 (after a pointless "Applying…").
    const ethKeys = ['ethernet', 'eth_address', 'eth_gateway', 'eth_dns'];
    const lockEth = managed(...ethKeys);
    const mode = h('select', { id: 'eth-mode', disabled: lockEth },
      h('option', { value: 'auto' }, 'Automatic'),
      h('option', { value: 'dhcp' }, 'DHCP'),
      h('option', { value: 'static' }, 'Static address'),
      h('option', { value: 'off' }, 'Disabled'));
    mode.value = e.mode || 'auto';
    // inputmode "decimal" would give phones a keypad without "/", which a CIDR address needs.
    const addr = h('input', { id: 'eth-addr', type: 'text', value: e.address || '', placeholder: '192.168.1.50/24', inputmode: 'text', autocapitalize: 'none', spellcheck: 'false', autocomplete: 'off', disabled: lockEth });
    const gw = h('input', { id: 'eth-gw', type: 'text', value: e.gateway || '', placeholder: '192.168.1.1', inputmode: 'decimal', autocomplete: 'off', disabled: lockEth });
    const dns = h('input', { id: 'eth-dns', type: 'text', value: (e.dns || []).join(', '), placeholder: '192.168.1.1, 1.1.1.1', autocomplete: 'off', disabled: lockEth });
    const staticBox = h('div', { class: 'static-fields' },
      h('div', { class: 'field' }, h('label', { for: 'eth-addr' }, 'Address (CIDR)'), addr),
      h('div', { class: 'field' }, h('label', { for: 'eth-gw' }, 'Gateway'), gw),
      h('div', { class: 'field' }, h('label', { for: 'eth-dns' }, 'DNS servers (comma separated)'), dns));
    const err = h('p', { class: 'form-error', role: 'alert' });
    const save = h('button', { id: 'eth-save', type: 'submit', class: 'btn btn-primary', disabled: lockEth }, 'Save');
    const progress = h('p', { class: 'muted small', role: 'status' });
    const sync = () => { staticBox.hidden = mode.value !== 'static'; };
    mode.addEventListener('change', sync);
    sync();
    const form = h('form', { onsubmit: async (ev) => {
      ev.preventDefault();
      if (lockEth || applyingEth) return;
      err.textContent = '';
      const body = { mode: mode.value, address: '', gateway: '', dns: [] };
      if (mode.value === 'static') {
        const dnsList = dns.value.split(/[,\s]+/).filter(Boolean);
        if (!CIDR.test(addr.value.trim())) { err.textContent = 'Address must look like 192.168.1.50/24.'; addr.focus(); return; }
        if (gw.value.trim() && !IPV4.test(gw.value.trim())) { err.textContent = 'Gateway must be an IPv4 address.'; gw.focus(); return; }
        if (dnsList.some((d) => !IPV4.test(d))) { err.textContent = 'DNS servers must be IPv4 addresses.'; dns.focus(); return; }
        body.address = addr.value.trim(); body.gateway = gw.value.trim(); body.dns = dnsList;
      }
      // Applying can take up to ~55 s on the device. Show progress in place (the card
      // is not re-rendered meanwhile, so what was typed survives a failure).
      applyingEth = true;
      save.disabled = true;
      replace(save, spinner(), 'Applying…');
      replace(progress, 'Applying the Ethernet settings can take up to a minute. If you reach the device over Ethernet, this page may lose contact for a moment.');
      let saved = false;
      try { await put('/api/network/ethernet', body, { timeout: LONG_TIMEOUT }); saved = true; toast('Ethernet settings saved'); } catch (er) { err.textContent = er.message; }
      applyingEth = false;
      save.disabled = lockEth;
      replace(save, 'Save');
      replace(progress);
      if (saved && alive) load({ force: true });
    } },
      h('div', { class: 'field' }, h('label', { for: 'eth-mode' }, 'Mode ', managedBadge('ethernet', 'eth_address', 'eth_gateway', 'eth_dns')), mode),
      staticBox, err, progress,
      h('div', { class: 'btn-row row-gap' }, save));
    return h('article', { class: 'card' },
      h('header', { class: 'card-head' }, h('h3', {}, 'Ethernet')),
      lockEth
        ? notice('info', icon('lock', 16), h('span', {}, 'The Ethernet setup is set in ', h('code', {}, 'emm.txt'), ' (', h('code', {}, EMM_KEYS.ethernet), ') and re-applied at every boot, so you can’t change it here. Edit the file on the SD card to change it.'))
        : null,
      form);
  }

  function managedCard() {
    const keys = (data && data.boot_managed) || [];
    if (!keys.length) return null;
    return h('article', { class: 'card' },
      h('header', { class: 'card-head' }, h('h3', {}, 'Managed by emm.txt')),
      h('p', {}, 'These values are set in ', h('code', {}, 'emm.txt'), ' on the SD card and re-applied at every boot, so changing them here won’t stick. Edit the file to change them:'),
      h('ul', { class: 'key-list' }, keys.map((k) => h('li', {}, h('code', {}, EMM_KEYS[k] || k)))));
  }

  // ---- join dialog
  function joinDialog(net) {
    const hiddenNet = !net;
    let gone = false; // the dialog was dismissed while the join was still running
    openModal((close, titleId) => {
      const ssid = h('input', { id: 'jn-ssid', type: 'text', value: net ? net.ssid : '', maxlength: '32', readOnly: !hiddenNet, required: true, autocapitalize: 'none', spellcheck: 'false', autocomplete: 'off' });
      const open = net && isOpenNet(net);
      const pw = h('input', { id: 'jn-pw', type: 'password', autocomplete: 'off', autocapitalize: 'none', spellcheck: 'false', maxlength: '64', 'aria-describedby': 'jn-hint', autofocus: !hiddenNet && !open });
      const show = h('button', { type: 'button', class: 'btn btn-small', 'aria-pressed': 'false', onclick: () => {
        const on = pw.type === 'password';
        pw.type = on ? 'text' : 'password';
        show.setAttribute('aria-pressed', String(on));
        replace(show, icon(on ? 'eyeOff' : 'eye', 14), on ? 'Hide' : 'Show');
      } }, icon('eye', 14), 'Show');
      const err = h('p', { class: 'form-error', role: 'alert' });
      const progress = h('p', { class: 'muted small', role: 'status' });
      const go = h('button', { type: 'submit', class: 'btn btn-primary' }, 'Join');
      const cancel = h('button', { type: 'button', class: 'btn', onclick: () => close(false) }, 'Cancel');
      const form = h('form', { onsubmit: async (e) => {
        e.preventDefault();
        if (go.disabled) return;
        err.textContent = '';
        // A scanned network is joined under exactly the name the scan gave (an SSID may
        // begin or end with a space); a typed one under exactly what was typed.
        const s = hiddenNet ? ssid.value : net.ssid;
        if (!s.trim()) { err.textContent = 'Enter the network name.'; ssid.focus(); return; }
        const p = pw.value;
        const problem = wifiPasswordProblem(p, { secured: !hiddenNet && !open });
        if (problem) { err.textContent = problem; pw.focus(); return; }
        // The device can take up to a minute to join (or give up on) a network.
        go.disabled = true;
        replace(go, spinner(), 'Joining…');
        replace(cancel, 'Close');
        ssid.readOnly = true;
        pw.readOnly = true;
        replace(progress, 'Joining can take up to a minute while the device tries the password. You can close this window; the Status card keeps showing the progress.');
        joiningSsid = s;
        render();
        try {
          const r = await post('/api/network/wifi', { ssid: s, password: p }, { timeout: LONG_TIMEOUT });
          joiningSsid = '';
          if (r && r.status) { data = { ...(data || {}), status: r.status }; }
          const done = r && r.status && r.status.mode === 'client' && r.status.ssid === s;
          toast(done ? `Connected to ${s}` : `Joining ${s}…`);
          // Rebuild the cards first: the join started a rebuild that replaced the control
          // that opened this dialog (and left it disabled), and closing the dialog hands
          // keyboard focus back to that control's successor.
          if (alive) render();
          close(true);
          if (alive) load();
        } catch (er) {
          joiningSsid = '';
          if (alive) render();
          if (gone) toast(er.message.includes(s) ? er.message : `Couldn't join ${s}: ${er.message}`, 'error');
          else {
            err.textContent = er.message;
            go.disabled = false;
            replace(go, 'Join');
            replace(cancel, 'Cancel');
            ssid.readOnly = !hiddenNet;
            pw.readOnly = false;
            replace(progress);
          }
        }
      } },
        h('h2', { id: titleId }, hiddenNet ? 'Join a hidden network' : `Join ${net.ssid}`),
        h('div', { class: 'field' }, h('label', { for: 'jn-ssid' }, 'Network name'), ssid),
        h('div', { class: 'field' },
          h('label', { for: 'jn-pw' }, open ? 'Password (not needed for open networks)' : 'Password'),
          h('div', { class: 'inline-form-row' }, pw, show),
          h('p', { id: 'jn-hint', class: 'muted small' }, hiddenNet ? 'Leave the password empty for an open network.' : (open ? 'This network is open.' : 'The password is stored on the device.'))),
        err, progress,
        h('div', { class: 'modal-actions' }, cancel, go));
      return h('div', { class: 'modal-body' }, form);
    }, { onClose: () => { gone = true; } });
  }

  // Held back while a text field has focus (unless `force`) and while an Ethernet
  // change is being applied (the form shows its progress and keeps what was typed).
  // A refresh that was held back is done once focus leaves the cards.
  function render({ force = false } = {}) {
    if (applyingEth || (editing && !force)) { pending = true; return; }
    pending = false;
    const saved = captureFocus(host);
    clear(host);
    if (!data) { host.append(h('p', { class: 'muted' }, 'Loading…')); return; }
    host.append(h('div', { class: 'cards' }, statusCard(), wifiCard(), ethernetCard(), managedCard()));
    restoreFocus(host, saved, { forget: 'join-hidden' });
  }

  host.addEventListener('focusin', (e) => { editing = isTextField(e.target); });
  host.addEventListener('focusout', () => {
    editing = false;
    // Not while focus merely moves between the cards' own controls: a rebuild between
    // mousedown and mouseup would swallow the click on a Save button.
    setTimeout(() => { if (alive && pending && !editing && !host.contains(document.activeElement)) render(); }, 0);
  });
  const offs = [on('network', (body) => { if (body && body.status) { data = body; networks = body.networks || networks; render(); schedulePoll(); } else load(); })];
  render();
  load();
  return () => { alive = false; offs.forEach((f) => f()); clearTimeout(pollTimer); };
}
