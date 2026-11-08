// Devices tab: one card per physical device, plus the network (RTP-MIDI)
// ports and the Bluetooth port.

import { h, clear, replace, fmtBytes, fmtNum } from './dom.js';
import { icon } from './icons.js';
import { get, post, patch, del } from './api.js';
import { S, on, emit, displayName, isNetworkPort } from './state.js';
import { led } from './leds.js';
import { toast, badge, spinner, toggleSwitch, confirmDialog, isTextField } from './ui.js';

const KIND_LABEL = { usb: 'USB', rtp: 'RTP-MIDI', ble: 'Bluetooth', virtual: 'Virtual', sim: 'Simulated' };
const portUrl = (id) => `/api/ports/${id}`; // ids are [A-Za-z0-9:._-] so no escaping is needed

export function mount(root) {
  const host = h('section', { class: 'devices' });
  root.append(host);

  let rtpList = [];
  let bleStatus = null;
  let sig = null;
  let dirty = false;
  const statEls = new Map(); // port id -> element
  let statQueue = new Set();
  let raf = 0;

  // ---- data
  let rtpTimer = 0;
  async function loadRtp() {
    try { rtpList = (await get('/api/rtp')) || []; } catch (_) { rtpList = []; }
    render(true);
  }
  async function loadBle() {
    try { bleStatus = ((await get('/api/settings')) || {}).ble || null; } catch (_) { bleStatus = null; }
    render(true);
  }
  function scheduleRtp() { clearTimeout(rtpTimer); rtpTimer = setTimeout(loadRtp, 300); }

  // ---- stats (updated in place on activity, one DOM write per frame)
  function statsText(p) {
    const s = p.stats || {};
    const bits = [];
    if (p.input) bits.push(`rx ${fmtNum(s.rx_msgs)} · ${fmtBytes(s.rx_bytes)}`);
    if (p.output) bits.push(`tx ${fmtNum(s.tx_msgs)} · ${fmtBytes(s.tx_bytes)}`);
    if (s.queued_bytes) bits.push(`queued ${fmtBytes(s.queued_bytes)}`);
    return bits.join('   ');
  }
  function paintStats(p, el) {
    const s = p.stats || {};
    replace(el,
      h('span', {}, statsText(p)),
      s.dropped ? h('span', { class: 'stat-warn', title: 'Messages dropped because the destination could not keep up' }, `   dropped ${fmtNum(s.dropped)}`) : null,
      s.errors ? h('span', { class: 'stat-warn', title: 'Write errors' }, `   errors ${fmtNum(s.errors)}`) : null);
  }
  function flushStats() {
    raf = 0;
    for (const id of statQueue) {
      const el = statEls.get(id), p = S.byId.get(id);
      if (el && p && el.isConnected) paintStats(p, el);
    }
    statQueue = new Set();
  }

  // ---- pieces
  function aliasEditor(p) {
    const wrap = h('div', { class: 'alias' });
    function show() {
      replace(wrap,
        h('span', { class: 'alias-name', title: p.name }, displayName(p)),
        p.alias ? h('small', { class: 'muted' }, `(${p.name})`) : null,
        h('button', { class: 'icon-btn', type: 'button', 'aria-label': `Rename ${displayName(p)}`, title: 'Rename', onclick: edit }, icon('pencil', 14)));
    }
    function edit() {
      const input = h('input', { type: 'text', value: p.alias || p.name, maxlength: '60', 'aria-label': 'Port name', class: 'alias-input' });
      let done = false;
      const finish = async (save) => {
        if (done) return;
        done = true;
        if (save) {
          let v = input.value.trim();
          if (v === p.name) v = '';
          if (v !== (p.alias || '')) {
            const live = S.byId.get(p.id), old = live ? live.alias : p.alias;
            if (live) live.alias = v;
            p.alias = v;
            emit('topology', { local: true });
            try { await patch(portUrl(p.id), { alias: v }); } catch (e) {
              if (live) live.alias = old;
              p.alias = old;
              emit('topology', { local: true });
              toast(`Couldn't rename: ${e.message}`, 'error');
            }
          }
        }
        dirty = false;
        show();
      };
      input.addEventListener('keydown', (e) => {
        if (e.key === 'Enter') { e.preventDefault(); finish(true); }
        if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); finish(false); }
      });
      replace(wrap, input,
        h('button', { class: 'icon-btn', type: 'button', 'aria-label': 'Save name', onclick: () => finish(true) }, icon('check', 16)),
        h('button', { class: 'icon-btn', type: 'button', 'aria-label': 'Cancel', onclick: () => finish(false) }, icon('x', 16)));
      input.focus();
      input.select();
    }
    show();
    return wrap;
  }

  // `disabled` would blur the button the keyboard user just pressed, so it is only
  // marked aria-disabled (same look) and ignores clicks while it works.
  function actionButton(label, ic, run, fk) {
    const b = h('button', { type: 'button', class: 'btn btn-small', dataset: { fk } }, icon(ic, 14), label);
    let busy = false;
    b.addEventListener('click', async () => {
      if (busy) return;
      busy = true;
      b.setAttribute('aria-disabled', 'true');
      const sp = spinner();
      b.prepend(sp);
      const started = Date.now();
      try { await run(); } catch (e) { toast(e.message, 'error'); } finally {
        setTimeout(() => { sp.remove(); b.removeAttribute('aria-disabled'); busy = false; }, Math.max(0, 450 - (Date.now() - started)));
      }
    });
    return b;
  }

  function portRow(p) {
    const stats = h('div', { class: 'stats mono' });
    statEls.set(p.id, stats);
    paintStats(p, stats);
    const offline = p.online === false;
    const actions = [];
    if (p.kind === 'usb' || p.kind === 'sim') {
      actions.push(toggleSwitch({
        fk: `rtp:${p.id}`, checked: !!p.rtp, label: 'Network port', hint: 'RTP-MIDI',
        onChange: (v) => patch(portUrl(p.id), { rtp: v }),
      }));
    }
    actions.push(toggleSwitch({
      fk: `hide:${p.id}`, checked: !!p.hidden, label: 'Hide in matrix',
      onChange: (v) => patch(portUrl(p.id), { hidden: v }),
    }));
    const buttons = [];
    if (p.output && !offline) {
      buttons.push(actionButton('Test note', 'note', async () => { await post(`${portUrl(p.id)}/test-note`); }, `note:${p.id}`));
      buttons.push(actionButton('Panic', 'panic', async () => { await post(`${portUrl(p.id)}/panic`); toast(`Sent all-notes-off to ${displayName(p)}`); }, `panic:${p.id}`));
    }
    return h('li', { class: 'port' + (offline ? ' is-offline' : '') },
      h('div', { class: 'port-main' },
        p.input ? led(p.id, 'rx') : null,
        p.output ? led(p.id, 'tx') : null,
        aliasEditor(p),
        offline ? badge('offline', 'warn') : null,
        p.hidden ? badge('hidden') : null),
      p.detail ? h('div', { class: 'port-detail muted small mono' }, p.detail) : null,
      offline ? null : stats,
      h('div', { class: 'port-actions' }, ...actions, h('div', { class: 'btn-row' }, ...buttons)));
  }

  function deviceCards(ports) {
    const groups = new Map();
    for (const p of ports) {
      const k = p.group || p.name || p.id;
      if (!groups.has(k)) groups.set(k, []);
      groups.get(k).push(p);
    }
    return Array.from(groups, ([name, list]) => {
      const kinds = Array.from(new Set(list.map((p) => KIND_LABEL[p.kind] || p.kind)));
      const allOffline = list.every((p) => p.online === false);
      return h('article', { class: 'card device' + (allOffline ? ' is-offline' : '') },
        h('header', { class: 'card-head' },
          h('h3', {}, name),
          h('div', { class: 'badges' }, ...kinds.map((k) => badge(k, 'kind')), allOffline ? badge('not connected', 'warn') : null)),
        h('ul', { class: 'ports' }, list.map(portRow)));
    });
  }

  function networkSection(ports) {
    const byId = new Map(rtpList.map((r) => [r.id, r]));
    const rows = ports.map((p) => {
      const info = byId.get(p.id);
      const dev = info && info.device ? S.byId.get(info.device) : null;
      const peers = info ? info.peers : null;
      return h('li', { class: 'port' + (p.online === false ? ' is-offline' : '') },
        h('div', { class: 'port-main' },
          led(p.id, 'rx'), led(p.id, 'tx'),
          aliasEditor(p),
          peers != null ? badge(peers === 0 ? 'no peers' : `${peers} peer${peers === 1 ? '' : 's'}`, peers ? 'ok' : '') : null,
          info && !info.device ? badge('standalone') : null),
        h('div', { class: 'port-detail muted small mono' },
          info ? `UDP ${info.control_port} (data ${info.control_port + 1})` : p.detail || '',
          dev ? `  ·  linked to ${displayName(dev)}` : ''),
        (() => { const el = h('div', { class: 'stats mono' }); statEls.set(p.id, el); paintStats(p, el); return el; })(),
        info && !info.device ? h('div', { class: 'port-actions' }, h('button', {
          type: 'button', class: 'btn btn-small btn-danger',
          onclick: async () => {
            if (!(await confirmDialog({ title: 'Remove network port?', body: `“${displayName(p)}” and its connections will be deleted.`, confirmLabel: 'Remove', danger: true }))) return;
            try { await del(`/api/rtp/ports/${p.id}`); toast('Network port removed'); scheduleRtp(); } catch (e) { toast(e.message, 'error'); }
          },
        }, icon('trash', 14), 'Remove')) : null);
    });

    const nameInput = h('input', { type: 'text', id: 'newnet', placeholder: 'e.g. DAW bus', maxlength: '40', 'aria-label': 'Name for the new network port', required: true });
    const form = h('form', { class: 'inline-form', onsubmit: async (e) => {
      e.preventDefault();
      const name = nameInput.value.trim();
      if (!name) return;
      try { await post('/api/rtp/ports', { name }); nameInput.value = ''; toast(`Added network port “${name}”`); scheduleRtp(); } catch (er) { toast(er.message, 'error'); }
    } },
      h('label', { for: 'newnet' }, 'Add a network port'),
      h('div', { class: 'inline-form-row' }, nameInput, h('button', { type: 'submit', class: 'btn btn-primary' }, icon('plus', 14), 'Add')),
      h('p', { class: 'muted small' }, 'Every USB device already gets its own. Extra ports are handy as a bus: connect your DAW to one, then fan it out to several synths in the Matrix.'));

    return h('article', { class: 'card' },
      h('header', { class: 'card-head' }, h('h3', {}, icon('wifi', 16), ' Network ports (RTP-MIDI)'),
        h('div', { class: 'badges' }, badge('AppleMIDI / rtpMIDI', 'kind'))),
      rows.length ? h('ul', { class: 'ports' }, rows) : h('p', { class: 'muted' }, 'No network ports. Switch “Network port” on for a device above, or add one below.'),
      form);
  }

  function bleSection(ports) {
    if (!ports.length && !bleStatus) return null;
    const lines = [];
    if (bleStatus) {
      if (bleStatus.error) lines.push(h('p', { class: 'stat-warn' }, bleStatus.error));
      else if (!bleStatus.enabled) lines.push(h('p', { class: 'muted' }, 'Bluetooth MIDI is switched off (System tab).'));
      else if (bleStatus.available === false) lines.push(h('p', { class: 'muted' }, 'No Bluetooth adapter found.'));
      else lines.push(h('p', {}, bleStatus.advertising ? `Advertising as “${bleStatus.name}”` : 'Not advertising', ` · ${bleStatus.connected || 0} connected`));
    }
    return h('article', { class: 'card' },
      h('header', { class: 'card-head' }, h('h3', {}, icon('bluetooth', 16), ' Bluetooth LE MIDI'), h('div', { class: 'badges' }, badge('BLE', 'kind'))),
      ...lines,
      ports.length ? h('ul', { class: 'ports' }, ports.map(portRow)) : null,
      h('p', { class: 'muted small' }, 'Bluetooth MIDI has no cable numbers, so this is one port pair. It suits notes and controllers, not big SysEx dumps.'));
  }

  // ---- render
  function signature() {
    return JSON.stringify([S.ports.map((p) => [p.id, p.name, p.alias, p.online, p.rtp, p.hidden, p.group, p.detail]), rtpList, bleStatus]);
  }

  function render(force) {
    const s = signature();
    if (!force && s === sig) return;
    if (sig !== null && host.contains(document.activeElement) && isTextField(document.activeElement)) { dirty = true; return; }
    // The focused control is found again after the rebuild by its data-fk (switches, action
    // buttons) or its aria-label (rename buttons).
    const focusKey = (el) => el.getAttribute('data-fk') || el.getAttribute('aria-label');
    const keepFocus = document.activeElement && host.contains(document.activeElement) ? focusKey(document.activeElement) : null;
    sig = s;
    statEls.clear();
    clear(host);
    const hw = S.ports.filter((p) => !isNetworkPort(p));
    const net = S.ports.filter((p) => p.kind === 'rtp');
    const ble = S.ports.filter((p) => p.kind === 'ble');
    if (!hw.length) {
      host.append(h('div', { class: 'empty' }, icon('plug', 36), h('h2', {}, 'No MIDI devices yet'),
        h('p', {}, 'Plug a USB MIDI device into the hub. It appears here within a second or two.')));
    } else {
      host.append(h('div', { class: 'cards' }, deviceCards(hw)));
    }
    host.append(h('div', { class: 'cards' }, networkSection(net), bleSection(ble)));
    if (keepFocus) {
      const el = Array.from(host.querySelectorAll('[data-fk],[aria-label]')).find((e) => focusKey(e) === keepFocus);
      if (el) el.focus({ preventScroll: true });
    }
  }

  const offs = [
    on('topology', () => { render(false); scheduleRtp(); }),
    on('activity', ({ id }) => { statQueue.add(id); if (!raf) raf = requestAnimationFrame(flushStats); }),
    on('settings', loadBle),
  ];
  host.addEventListener('focusout', () => { setTimeout(() => { if (dirty && !host.contains(document.activeElement)) { dirty = false; render(false); } }, 0); });
  render(true);
  loadRtp();
  loadBle();
  return () => { offs.forEach((f) => f()); clearTimeout(rtpTimer); if (raf) cancelAnimationFrame(raf); };
}
