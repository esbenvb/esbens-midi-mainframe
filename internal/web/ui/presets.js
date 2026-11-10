// Presets: save the whole routing (every connection with its filters) under a
// name and bring it back later. The button lives in the Matrix tab's toolbar;
// it opens a popover with the list and a save form.

import { h, replace } from './dom.js';
import { icon } from './icons.js';
import { get, post, patch, del } from './api.js';
import { on } from './state.js';
import { toast, openPopover, confirmDialog } from './ui.js';

const when = (s) => (s ? new Date(s * 1000).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' }) : '');
const plural = (n, word) => `${n} ${word}${n === 1 ? '' : 's'}`;

export function presetsButton() {
  const btn = h('button', { type: 'button', class: 'btn', 'aria-haspopup': 'dialog', onclick: () => openPresets(btn) },
    icon('bookmark', 15), 'Presets');
  return btn;
}

let openPop = null; // the popover that is open, so the button toggles it instead of stacking another

// Called when the Matrix tab is left: the popover hangs off <body>, not off the tab.
export function closePresets() { if (openPop) openPop.close(); }

function openPresets(anchor) {
  if (openPop) { openPop.close(); return; }
  let list = [];
  let editing = null; // id of the preset being renamed
  const listEl = h('ul', { class: 'presets' });
  let pop = null;

  async function load() {
    try { list = (await get('/api/presets')) || []; } catch (e) { toast(`Couldn't read the presets: ${e.message}`, 'error'); }
    paint();
  }

  async function recall(p) {
    const ok = await confirmDialog({
      title: `Recall “${p.name}”?`,
      body: `The current routing is replaced by the ${plural(p.connections, 'connection')} saved in this preset. Connections that are not in it are removed.`,
      confirmLabel: 'Recall',
    });
    if (!ok) return;
    try {
      const r = await post(`/api/presets/${p.id}/recall`);
      const skipped = (r.skipped || []).length;
      toast(`Recalled “${r.name}”: ${plural(r.applied, 'connection')}` + (skipped ? `, ${skipped} skipped (a device is gone or changed)` : ''), skipped ? 'info' : 'info');
      if (pop) pop.close();
    } catch (e) { toast(`Couldn't recall: ${e.message}`, 'error'); }
  }

  async function remove(p) {
    if (!(await confirmDialog({ title: `Delete “${p.name}”?`, body: 'The preset is deleted. Your current routing is not affected.', confirmLabel: 'Delete', danger: true }))) return;
    try { list = (await del(`/api/presets/${p.id}`)) || list; paint(); } catch (e) { toast(e.message, 'error'); }
  }

  function renameRow(p) {
    const input = h('input', { type: 'text', value: p.name, maxlength: '40', 'aria-label': `New name for ${p.name}`, class: 'alias-input' });
    let done = false;
    const finish = async (save) => {
      if (done) return;
      done = true;
      editing = null;
      const v = input.value.trim();
      if (save && v && v !== p.name) {
        try { list = (await patch(`/api/presets/${p.id}`, { name: v })) || list; } catch (e) { toast(e.message, 'error'); }
      }
      paint();
    };
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') { e.preventDefault(); finish(true); }
      if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); finish(false); }
    });
    queueMicrotask(() => { input.focus(); input.select(); });
    return h('li', { class: 'preset' },
      input,
      h('button', { class: 'icon-btn', type: 'button', 'aria-label': 'Save name', onclick: () => finish(true) }, icon('check', 16)),
      h('button', { class: 'icon-btn', type: 'button', 'aria-label': 'Cancel', onclick: () => finish(false) }, icon('x', 16)));
  }

  function row(p) {
    if (editing === p.id) return renameRow(p);
    return h('li', { class: 'preset' },
      h('div', { class: 'preset-main' },
        h('span', { class: 'preset-name' }, p.name),
        h('small', { class: 'muted' }, `${plural(p.connections, 'connection')} · ${when(p.saved)}`)),
      h('button', { type: 'button', class: 'btn btn-small btn-primary', onclick: () => recall(p), 'aria-label': `Recall ${p.name}` }, 'Recall'),
      h('button', { type: 'button', class: 'icon-btn', 'aria-label': `Rename ${p.name}`, title: 'Rename', onclick: () => { editing = p.id; paint(); } }, icon('pencil', 14)),
      h('button', { type: 'button', class: 'icon-btn', 'aria-label': `Delete ${p.name}`, title: 'Delete', onclick: () => remove(p) }, icon('trash', 14)));
  }

  function paint() {
    replace(listEl, list.length ? list.map(row) : h('li', { class: 'muted small' }, 'No presets yet. Set up the routing you like, then save it below.'));
    if (pop) pop.place();
  }

  const nameInput = h('input', { type: 'text', id: 'preset-name', maxlength: '40', placeholder: 'e.g. Studio, Live set', 'aria-label': 'Name for the new preset' });
  async function save(replaceExisting) {
    const name = nameInput.value.trim();
    if (!name) { toast('Give the preset a name.', 'error'); nameInput.focus(); return; }
    try {
      await post('/api/presets', { name, replace: !!replaceExisting });
      nameInput.value = '';
      toast(`Saved preset “${name}”`);
      await load();
    } catch (e) {
      if (e.status === 409 && !replaceExisting && /already exists/.test(e.message)) {
        if (await confirmDialog({ title: `Replace “${name}”?`, body: 'A preset with this name exists. Replace it with the current routing?', confirmLabel: 'Replace', danger: true })) save(true);
      } else toast(e.message, 'error');
    }
  }
  const form = h('form', { class: 'inline-form', onsubmit: (e) => { e.preventDefault(); save(false); } },
    h('label', { for: 'preset-name' }, 'Save the current routing as'),
    h('div', { class: 'inline-form-row' }, nameInput, h('button', { type: 'submit', class: 'btn btn-primary' }, 'Save')));

  const content = h('div', { class: 'presets-pop' },
    h('h3', {}, 'Presets'),
    h('p', { class: 'muted small' }, 'A preset is the whole routing: every connection and its filters. Device names, hidden ports and network switches are not part of it.'),
    listEl, form);
  paint();
  const off = on('presets', (body) => { if (Array.isArray(body)) { list = body; if (!editing) paint(); } });
  pop = openPop = openPopover(anchor, content, { label: 'Presets', onClose: () => { off(); openPop = null; } });
  load();
}
