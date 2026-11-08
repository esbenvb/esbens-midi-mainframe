// Filter editor popover for one connection. Edits apply live (debounced PUT)
// and roll back with an inline error if the daemon rejects them.

import { h, debounce } from './dom.js';
import { icon } from './icons.js';
import { S, on, ck, displayName, nameOfId, normFilter, connect, disconnect, CATEGORIES, DEFAULT_FILTER } from './state.js';
import { openPopover, toast } from './ui.js';

const CHANNELS = Array.from({ length: 16 }, (_, i) => i + 1);

export function openFilterEditor(anchor, from, to) {
  const conn = S.connMap.get(ck(from, to));
  if (!conn) { toast('Connect the two ports first, then adjust the filter.', 'info'); return null; }

  const st = { block: new Set(), chans: new Set(CHANNELS), map: 0, tr: 0 };
  function load(f) {
    f = normFilter(f);
    st.block = new Set(f.block);
    st.chans = f.channels.length ? new Set(f.channels) : new Set(CHANNELS);
    st.map = f.map_channel;
    st.tr = f.transpose;
  }
  load(conn.filter);

  const errEl = h('p', { class: 'form-error', role: 'alert' });
  const catBtns = new Map();
  const chBtns = new Map();
  const mapSel = h('select', { id: 'fe-map', 'aria-label': 'Force channel' },
    h('option', { value: '0' }, 'Keep original'),
    ...CHANNELS.map((c) => h('option', { value: String(c) }, 'Channel ' + c)));
  const trInput = h('input', { id: 'fe-tr', type: 'number', min: '-48', max: '48', step: '1', inputmode: 'numeric', 'aria-label': 'Transpose in semitones' });

  let queue = Promise.resolve();
  const commit = debounce(() => {
    const filter = {
      block: Array.from(st.block),
      channels: st.chans.size === 16 ? [] : Array.from(st.chans).sort((a, b) => a - b),
      map_channel: st.map,
      transpose: st.tr,
    };
    queue = queue.then(async () => {
      try {
        await connect(from, to, filter);
        errEl.textContent = '';
      } catch (e) {
        errEl.textContent = `Couldn't save: ${e.message}`;
        const cur = S.connMap.get(ck(from, to));
        if (cur) load(cur.filter);
        paint();
      }
    });
  }, 250);

  function paint() {
    for (const [key, b] of catBtns) {
      const pass = !st.block.has(key);
      b.setAttribute('aria-pressed', String(pass));
      b.classList.toggle('on', pass);
    }
    for (const [c, b] of chBtns) {
      const on = st.chans.has(c);
      b.setAttribute('aria-pressed', String(on));
      b.classList.toggle('on', on);
    }
    mapSel.value = String(st.map);
    trInput.value = String(st.tr);
  }

  const catGrid = h('div', { class: 'tog-grid' });
  for (const [key, label, hint] of CATEGORIES) {
    const b = h('button', { type: 'button', class: 'tog', title: hint || label, 'aria-pressed': 'true', autofocus: key === 'note',
      onclick: () => { if (st.block.has(key)) st.block.delete(key); else st.block.add(key); paint(); commit(); } },
      h('span', { class: 'tog-mark', 'aria-hidden': 'true' }, icon('check', 11)),
      label);
    catBtns.set(key, b);
    catGrid.append(b);
  }

  const chGrid = h('div', { class: 'ch-grid' });
  for (const c of CHANNELS) {
    const b = h('button', { type: 'button', class: 'tog tog-ch', 'aria-pressed': 'true', 'aria-label': 'Channel ' + c,
      onclick: () => {
        if (st.chans.has(c)) {
          if (st.chans.size === 1) { toast('At least one channel must stay on. Block message types above to silence everything.', 'info'); return; }
          st.chans.delete(c);
        } else st.chans.add(c);
        paint(); commit();
      } }, String(c));
    chBtns.set(c, b);
    chGrid.append(b);
  }

  mapSel.addEventListener('change', () => { st.map = Number(mapSel.value) || 0; commit(); });
  function setTr(v) {
    v = Math.max(-48, Math.min(48, Math.round(Number(v) || 0)));
    st.tr = v; trInput.value = String(v); commit();
  }
  trInput.addEventListener('change', () => setTr(trInput.value));
  const stepBtn = (label, delta, aria) => h('button', { type: 'button', class: 'btn btn-small', 'aria-label': aria, onclick: () => setTr(st.tr + delta) }, label);

  let pop = null;
  const body = h('div', { class: 'fe' },
    h('div', { class: 'fe-head' },
      h('div', {},
        h('h3', {}, 'Filter'),
        h('p', { class: 'fe-route' }, nameOfId(from), ' ', h('span', { 'aria-label': 'to' }, '→'), ' ', nameOfId(to))),
      h('button', { class: 'icon-btn', type: 'button', 'aria-label': 'Close filter editor', onclick: () => pop && pop.close() }, icon('x', 16))),
    h('fieldset', {}, h('legend', {}, 'Message types ', h('small', {}, 'on = passed through')), catGrid),
    h('fieldset', {},
      h('legend', {}, 'Channels ', h('small', {}, 'allow-list')),
      chGrid,
      h('div', { class: 'row-gap' },
        h('button', { type: 'button', class: 'btn btn-small', onclick: () => { st.chans = new Set(CHANNELS); paint(); commit(); } }, 'All channels'))),
    h('div', { class: 'fe-two' },
      h('div', { class: 'field' }, h('label', { for: 'fe-map' }, 'Force channel'), mapSel),
      h('div', { class: 'field' }, h('label', { for: 'fe-tr' }, 'Transpose (semitones)'),
        h('div', { class: 'stepper' },
          stepBtn('−12', -12, 'Down an octave'), stepBtn('−', -1, 'Down a semitone'),
          trInput,
          stepBtn('+', 1, 'Up a semitone'), stepBtn('+12', 12, 'Up an octave')))),
    errEl,
    h('div', { class: 'fe-actions' },
      h('button', { type: 'button', class: 'btn', onclick: () => { load(DEFAULT_FILTER()); paint(); commit(); } }, 'Reset'),
      h('button', { type: 'button', class: 'btn btn-danger', onclick: async () => {
        commit.cancel();
        removing = true; // our own removal: the topology listener below must not announce it
        try { await disconnect(from, to); pop && pop.close(); } catch (e) { errEl.textContent = e.message; } finally { removing = false; }
      } }, icon('trash', 14), 'Disconnect')));

  // Another client (or the daemon) may remove this connection while the editor is open. Close
  // the editor and drop any pending edit: flushing it would silently re-create the connection.
  let removing = false;
  const offTopology = on('topology', () => {
    if (removing || S.connMap.has(ck(from, to))) return;
    commit.cancel();
    toast('That connection was removed, so its filter editor was closed.', 'info');
    if (pop) pop.close();
  });

  paint();
  pop = openPopover(anchor, body, {
    label: `Filter ${displayName(S.byId.get(from))} to ${displayName(S.byId.get(to))}`,
    onClose: () => { offTopology(); commit.flush(); },
  });
  // keep the popover in view if its height changes (error line appears)
  return pop;
}
