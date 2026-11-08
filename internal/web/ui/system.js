// System tab: device facts, settings, power.

import { h, replace, fmtUptime } from './dom.js';
import { icon } from './icons.js';
import { get, put, post } from './api.js';
import { S, on, emit } from './state.js';
import { toast, badge, notice, kv, toggleSwitch, confirmDialog, emmTxtBadge, isTextField, captureFocus, restoreFocus } from './ui.js';

export function mount(root) {
  const host = h('section', { class: 'system' });
  root.append(host);

  let sys = S.system;
  let settings = null;
  let timer = 0;
  let editing = false;  // focus is in a text field of the settings card: it is not rebuilt under the cursor
  let pending = false;  // ...so a refresh that arrives meanwhile waits until focus has left the card
  let alive = true; // false once the tab is left: a response that arrives later must not re-arm the poll

  async function loadSystem() {
    let fresh = null;
    try { fresh = await get('/api/system'); } catch (e) { /* keep the old values */ }
    if (!alive) return;
    if (fresh) { sys = fresh; S.system = sys; emit('system', sys); }
    renderInfo();
    clearTimeout(timer);
    timer = setTimeout(loadSystem, 5000);
  }
  async function loadSettings({ force = false } = {}) {
    let fresh = null;
    try { fresh = await get('/api/settings'); } catch (e) { if (alive) toast(`Couldn't read settings: ${e.message}`, 'error'); }
    if (!alive) return;
    if (fresh) settings = fresh;
    renderSettings({ force });
  }
  // After a save the card is refreshed even though the field that was submitted
  // (Enter) still has focus: it must show the new state, and the field comes back
  // with focus (by id). Without `force` that refresh would be held back.
  async function save(patch) {
    try {
      settings = (await put('/api/settings', patch)) || settings;
    } catch (e) {
      if (e.status === 409 && alive) loadSettings({ force: true }); // emm.txt fixes this setting: pick up the read-only state
      throw e;
    }
    if (alive) loadSettings({ force: true });
  }

  function infoCard() {
    if (!sys) return h('article', { class: 'card' }, h('p', { class: 'muted' }, 'Loading…'));
    const hot = typeof sys.cpu_temp_c === 'number' && sys.cpu_temp_c >= 75;
    return h('article', { class: 'card' },
      h('header', { class: 'card-head' }, h('h3', {}, 'This device'), h('div', { class: 'badges' }, badge('v' + (sys.version || '?'), 'kind'))),
      sys.state_writable === false ? notice('warn', icon('alert', 16), h('span', {}, 'The settings storage is read-only or unreadable, so changes will not survive a restart.')) : null,
      kv([
        ['Model', sys.model],
        ['Kernel', sys.kernel],
        ['Uptime', fmtUptime(sys.uptime_s)],
        ['CPU temperature', typeof sys.cpu_temp_c === 'number' ? h('span', { class: hot ? 'stat-warn' : '' }, `${sys.cpu_temp_c.toFixed(1)} °C${hot ? ' (hot)' : ''}`) : null],
        ['USB devices', sys.usb_devices],
        ['MIDI backend', h('code', {}, sys.backend)],
        ['Settings storage', sys.state_writable === false ? 'read-only or unreadable' : 'writable'],
      ]));
  }

  function settingsCard() {
    if (!settings) return h('article', { class: 'card' }, h('h3', {}, 'Settings'), h('p', { class: 'muted' }, 'Loading…'));
    const ble = settings.ble || {};
    const rtp = settings.rtp || {};
    const web = settings.web || {};
    // Settings that emm.txt fixes: shown read-only (the daemon answers 409 to a change).
    // The field is absent on older daemons, which means nothing is fixed.
    const fixed = settings.managed || {};
    const fixedText = (key) => `Set by emm.txt (${key}). Edit the file on the SD card to change it.`;

    const bleName = h('input', { id: 'ble-name', type: 'text', value: ble.name || '', maxlength: '20', disabled: !!fixed.ble_name, 'aria-describedby': 'ble-hint' });
    const bleForm = h('form', { class: 'inline-form', onsubmit: async (e) => {
      e.preventDefault();
      if (fixed.ble_name) return;
      const v = bleName.value.trim();
      if (!v) { toast('Give the Bluetooth device a name.', 'error'); return; }
      try { await save({ ble: { name: v } }); toast('Bluetooth name saved'); } catch (er) { toast(er.message, 'error'); }
    } },
      h('label', { for: 'ble-name' }, 'Bluetooth device name ', fixed.ble_name ? emmTxtBadge() : null),
      h('div', { class: 'inline-form-row' }, bleName, h('button', { id: 'ble-save', type: 'submit', class: 'btn', disabled: !!fixed.ble_name }, 'Save')),
      // Same order as the Devices tab: a real error, then "switched off" (the adapter reads as
      // unavailable whenever Bluetooth MIDI is off), then a missing adapter.
      h('p', { id: 'ble-hint', class: 'muted small' }, fixed.ble_name ? fixedText('ble_name') + ' ' : null,
        ble.error ? ble.error
          : !ble.enabled ? 'Bluetooth MIDI is off.'
          : ble.available === false ? 'No Bluetooth adapter found.'
          : ble.advertising ? `Advertising · ${ble.connected || 0} connected` : 'Not advertising yet'));

    const pw = h('input', { id: 'web-pw', type: 'password', autocomplete: 'new-password', placeholder: web.password_set ? 'New password' : 'Choose a password', disabled: !!fixed.web_password, 'aria-describedby': 'pw-hint' });
    const pwForm = h('form', { class: 'inline-form', onsubmit: async (e) => {
      e.preventDefault();
      if (fixed.web_password) return;
      if (pw.value.length < 4) { toast('Use at least 4 characters.', 'error'); return; }
      try { await save({ web: { password: pw.value } }); pw.value = ''; toast('Password set. Your browser will ask for it next time.'); } catch (er) { toast(er.message, 'error'); }
    } },
      h('label', { for: 'web-pw' }, 'Web interface password ', fixed.web_password ? emmTxtBadge() : null),
      h('div', { class: 'inline-form-row' }, pw,
        h('button', { id: 'pw-set', type: 'submit', class: 'btn', disabled: !!fixed.web_password }, web.password_set ? 'Change' : 'Set'),
        web.password_set ? h('button', { id: 'pw-remove', type: 'button', class: 'btn btn-danger', disabled: !!fixed.web_password, onclick: async () => {
          if (!(await confirmDialog({ title: 'Remove the password?', body: 'Anyone on your network will be able to open this page and re-route your MIDI.', confirmLabel: 'Remove password', danger: true }))) return;
          try { await save({ web: { password: '' } }); toast('Password removed'); } catch (er) { toast(er.message, 'error'); }
        } }, 'Remove') : null),
      h('p', { id: 'pw-hint', class: 'muted small' }, fixed.web_password ? fixedText('web_password') : web.password_set ? 'A password is set.' : 'No password: anyone on your network can use this page. AppleMIDI sessions are unauthenticated too, so keep the device on a network you trust.'));

    return h('article', { class: 'card' },
      h('header', { class: 'card-head' }, h('h3', {}, 'Settings')),
      h('div', { class: 'setting' }, toggleSwitch({ id: 'sw-rtp', checked: !!rtp.enabled, label: 'Network MIDI (RTP-MIDI)', disabled: !!fixed.rtp,
        hint: fixed.rtp ? fixedText('rtp') : 'Show each device as a network MIDI session',
        onChange: (v) => save({ rtp: { enabled: v } }) })),
      h('div', { class: 'setting' }, toggleSwitch({ id: 'sw-ble', checked: !!ble.enabled, label: 'Bluetooth MIDI', disabled: !!fixed.ble,
        hint: fixed.ble ? fixedText('ble') : 'Appear as a Bluetooth LE MIDI device',
        onChange: (v) => save({ ble: { enabled: v } }) })),
      bleForm,
      pwForm);
  }

  function powerCard() {
    if (!sys || !sys.allow_power) return null;
    const act = (label, path, ic, body, done) => h('button', { type: 'button', class: 'btn btn-danger', onclick: async () => {
      if (!(await confirmDialog({ title: `${label}?`, body, confirmLabel: label, danger: true }))) return;
      try { await post(path); toast(done); } catch (e) { toast(e.message, 'error'); }
    } }, icon(ic, 14), label);
    return h('article', { class: 'card' },
      h('header', { class: 'card-head' }, h('h3', {}, 'Power')),
      h('p', { class: 'muted' }, 'Active connections are released first, so no notes get stuck.'),
      h('div', { class: 'btn-row' },
        act('Reboot', '/api/system/reboot', 'refresh', 'The device restarts and this page reconnects in about a minute.', 'Rebooting…'),
        act('Shut down', '/api/system/shutdown', 'power', 'The device powers down. Unplug and replug it to start it again.', 'Shutting down…')));
  }

  // Three independent slots, so the 5 s refresh of the facts never rebuilds
  // (and steals focus from) the settings forms or the power buttons.
  const infoSlot = h('div');
  const settingsSlot = h('div');
  const powerSlot = h('div');
  host.append(h('div', { class: 'cards' }, infoSlot, settingsSlot, powerSlot));

  // The Power card only depends on allow_power, which does not change while the
  // daemon runs: build it once (or when that flips), not on every tick. Rebuilt
  // buttons would drop keyboard focus, and the confirm dialog's opener.
  let powerShown; // undefined until the first render
  function renderInfo() {
    replace(infoSlot, infoCard());
    const want = !!(sys && sys.allow_power);
    if (want !== powerShown) { powerShown = want; replace(powerSlot, powerCard()); }
  }
  function renderSettings({ force = false } = {}) {
    if (editing && !force) { pending = true; return; }
    pending = false;
    const saved = captureFocus(settingsSlot);
    replace(settingsSlot, settingsCard());
    restoreFocus(settingsSlot, saved, { 'pw-remove': 'web-pw' });
  }

  settingsSlot.addEventListener('focusin', (e) => { editing = isTextField(e.target); });
  settingsSlot.addEventListener('focusout', () => {
    editing = false;
    // Apply a refresh that was held back, once focus has left the card (not while it
    // moves between the card's own controls: a rebuild between mousedown and mouseup
    // would swallow the click on a Save button).
    setTimeout(() => { if (alive && pending && !editing && !settingsSlot.contains(document.activeElement)) renderSettings(); }, 0);
  });
  const offs = [on('settings', () => loadSettings())];
  renderInfo();
  renderSettings();
  loadSystem();
  loadSettings();
  return () => { alive = false; offs.forEach((f) => f()); clearTimeout(timer); };
}
