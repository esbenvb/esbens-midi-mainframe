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

  // ---- SSH (device image only: /api/ssh says available:false elsewhere)
  let ssh = null;
  async function loadSsh({ force = true } = {}) {
    try { ssh = await get('/api/ssh'); } catch (e) { /* keep the old values */ }
    if (alive) renderSsh({ force });
  }
  async function saveSsh(body) {
    ssh = (await put('/api/ssh', body)) || ssh;
    if (alive) renderSsh({ force: true });
  }

  function sshCard() {
    if (!ssh || !ssh.available) return null;
    const pw = h('input', { id: 'ssh-pw', type: 'password', autocomplete: 'new-password', placeholder: ssh.password_set ? 'New password (leave empty to keep)' : 'Choose a password (8+ characters)', 'aria-describedby': 'ssh-hint' });
    const keys = h('textarea', { id: 'ssh-keys', rows: '3', spellcheck: 'false', autocomplete: 'off', placeholder: 'ssh-ed25519 AAAA… you@laptop', 'aria-label': 'Public keys allowed to log in as root' });
    keys.value = (ssh.keys || []).join('\n');
    const keysDirty = () => keys.value.trim() !== (ssh.keys || []).join('\n').trim();
    // What the form holds, to be sent with the switch or the Save button.
    const typed = () => {
      const b = {};
      if (pw.value) b.password = pw.value;
      if (keysDirty()) b.authorized_keys = keys.value;
      return b;
    };
    const form = h('form', { class: 'inline-form', onsubmit: async (e) => {
      e.preventDefault();
      const b = typed();
      if (!Object.keys(b).length) { toast('Nothing to save.'); return; }
      try { await saveSsh(b); toast('SSH login saved'); } catch (er) { toast(er.message, 'error'); }
    } },
      h('label', { for: 'ssh-pw' }, 'Password for the user “root” ', ssh.password_set ? badge('set', 'ok') : null),
      h('div', { class: 'inline-form-row' }, pw),
      h('label', { for: 'ssh-keys' }, 'Public keys (optional, one per line) ', ssh.keys && ssh.keys.length ? badge(`${ssh.keys.length} saved`, 'ok') : null),
      keys,
      h('div', { class: 'btn-row' },
        h('button', { id: 'ssh-save', type: 'submit', class: 'btn' }, 'Save'),
        ssh.password_set ? h('button', { id: 'ssh-pw-remove', type: 'button', class: 'btn btn-danger', onclick: async () => {
          if (ssh.enabled && !(ssh.keys && ssh.keys.length)) { toast('Switch SSH off first, or add a public key: SSH needs a way to log in.', 'error'); return; }
          try { await saveSsh({ password: '' }); toast('SSH password removed'); } catch (er) { toast(er.message, 'error'); }
        } }, 'Remove password') : null),
      h('p', { id: 'ssh-hint', class: 'muted small' }, 'You need a password or a key (or both). The password is stored hashed; keys are public.'));

    return h('article', { class: 'card' },
      h('header', { class: 'card-head' }, h('h3', {}, icon('terminal', 16), ' SSH access'),
        h('div', { class: 'badges' }, ssh.enabled ? badge(ssh.running ? 'on' : 'starting…', ssh.running ? 'ok' : 'warn') : badge('off'))),
      ssh.needs_web_password ? notice('warn', icon('alert', 16), h('span', {}, 'Set a password for this web interface first (Settings above). SSH gives a root shell, so it stays locked until the web interface is protected too.')) : null,
      h('div', { class: 'setting' }, toggleSwitch({
        id: 'sw-ssh', checked: !!ssh.enabled, label: 'Allow SSH login', disabled: !!ssh.needs_web_password && !ssh.enabled,
        hint: 'Off by default. Nothing listens on port 22 while this is off.',
        onChange: async (v) => { await saveSsh({ enabled: v, ...typed() }); toast(v ? 'SSH is on' : 'SSH is off'); },
      })),
      form,
      ssh.enabled ? kv([
        ['Log in with', h('code', {}, `ssh root@${ssh.host}`)],
        ['Host key', ssh.fingerprint ? h('code', { class: 'mono-block' }, ssh.fingerprint) : null],
      ]) : null,
      ssh.enabled ? h('p', { class: 'muted small' }, 'SSH stays on across reboots until you switch it off here. It is a full root shell: keep this device on a network you trust.') : null);
  }

  // ---- backup and restore of the application state --------------------------
  // The file holds the routing, presets and device names. The network setup, the web
  // password and SSH are the device's own and are neither in it nor restored from it.
  let restoreResult = null; // what the last restore did, shown until the tab is left
  let restoring = false;
  function backupCard() {
    const picker = h('input', { id: 'restore-file', type: 'file', accept: '.json,application/json', hidden: true, 'aria-label': 'Backup file to restore', onchange: () => restoreFrom(picker) });
    const r = restoreResult;
    const skipped = r && Array.isArray(r.skipped) ? r.skipped : [];
    const warnings = r && Array.isArray(r.warnings) ? r.warnings : [];
    const unsaved = r && r.saved === false;
    const why = [...new Set(skipped.map((x) => x.reason).filter(Boolean))].slice(0, 3);
    const trouble = skipped.length || warnings.length || unsaved;
    return h('article', { class: 'card' },
      h('header', { class: 'card-head' }, h('h3', {}, icon('download', 16), ' Backup')),
      h('p', { class: 'muted' }, 'Save your routing, presets and device names to a file, and bring them back later or on another unit. The network setup, web password and SSH settings are not in it and cannot be restored from it: set those up on the device, or put them on the card when you flash it.'),
      h('div', { class: 'btn-row' },
        h('a', { id: 'backup-download', class: 'btn', href: '/api/backup', download: true }, icon('download', 14), 'Download backup'),
        h('button', { id: 'backup-restore', type: 'button', class: 'btn', disabled: restoring, onclick: () => picker.click() }, icon('upload', 14), restoring ? 'Restoring…' : 'Restore from file…'),
        picker),
      // role=status: a screen reader announces the result, not only the toast.
      r ? h('div', { role: 'status' }, notice(trouble ? 'warn' : 'info', icon(trouble ? 'alert' : 'check', 16),
        h('div', {},
          h('div', {}, `Restored ${r.connections} connection${r.connections === 1 ? '' : 's'} and ${r.presets} preset${r.presets === 1 ? '' : 's'}.`),
          skipped.length ? h('div', {}, `${skipped.length} connection${skipped.length === 1 ? ' was' : 's were'} left out${why.length ? ': ' + why.join('; ') : ''}.`) : null,
          unsaved ? h('div', {}, `It could not be saved to the card, so it will be lost when the device restarts${r.save_error ? ' (' + r.save_error + ')' : ''}.`) : null,
          warnings.map((w) => h('div', {}, w))))) : null);
  }
  function renderBackup() {
    if (!alive) return;
    const saved = captureFocus(backupSlot);
    replace(backupSlot, backupCard());
    restoreFocus(backupSlot, saved);
  }
  async function restoreFrom(picker) {
    const f = picker.files && picker.files[0];
    picker.value = ''; // so that the same file can be chosen again
    if (!f || restoring) return;
    if (f.size > 8 * 1024 * 1024) { toast('That file is too large to be an EMM backup.', 'error'); return; }
    let doc;
    try { doc = JSON.parse(await f.text()); } catch (e) { toast('That is not an EMM backup (not valid JSON).', 'error'); return; }
    if (!doc || doc.format !== 'emm-backup') { toast('That is not an EMM backup.', 'error'); return; }
    const st = doc.state || {};
    const n = (a) => (Array.isArray(a) ? a.length : 0);
    const when = doc.created ? new Date(doc.created).toLocaleString() : 'an unknown date';
    const ok = await confirmDialog({
      title: 'Restore this backup?',
      body: `Backup from ${doc.host || 'a device'}, ${when}: ${n(st.connections)} connection${n(st.connections) === 1 ? '' : 's'}, ${n(st.presets)} preset${n(st.presets) === 1 ? '' : 's'}. It replaces your current routing, presets and device names. The network setup, web password and SSH settings are not touched.`,
      confirmLabel: 'Restore', danger: true,
    });
    if (!ok) return;
    restoreResult = null; // a failed restore must not sit next to the notice of an earlier one
    restoring = true;
    renderBackup();
    try {
      restoreResult = await post('/api/restore', doc);
      if (restoreResult.saved === false) toast('Restored, but it could not be saved to the card', 'error');
      else toast('Backup restored');
    } catch (e) {
      toast(e.message, 'error');
    }
    restoring = false;
    renderBackup();
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
  const sshSlot = h('div');
  const backupSlot = h('div');
  const powerSlot = h('div');
  host.append(h('div', { class: 'cards' }, infoSlot, settingsSlot, sshSlot, backupSlot, powerSlot));

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

  // The SSH card is rebuilt only after its own actions (and when the web password
  // changes), never on a timer, so typing in it is not interrupted.
  let sshEditing = false;
  function renderSsh({ force = false } = {}) {
    if (sshEditing && !force) return; // somebody is typing: a refresh from elsewhere waits
    const saved = captureFocus(sshSlot);
    replace(sshSlot, sshCard());
    restoreFocus(sshSlot, saved, { 'ssh-pw-remove': 'ssh-pw' });
  }
  sshSlot.addEventListener('focusin', (e) => { sshEditing = isTextField(e.target); });
  sshSlot.addEventListener('focusout', () => { sshEditing = false; });

  settingsSlot.addEventListener('focusin', (e) => { editing = isTextField(e.target); });
  settingsSlot.addEventListener('focusout', () => {
    editing = false;
    // Apply a refresh that was held back, once focus has left the card (not while it
    // moves between the card's own controls: a rebuild between mousedown and mouseup
    // would swallow the click on a Save button).
    setTimeout(() => { if (alive && pending && !editing && !settingsSlot.contains(document.activeElement)) renderSettings(); }, 0);
  });
  const offs = [on('settings', () => { loadSettings(); loadSsh({ force: false }); })];
  renderInfo();
  renderSettings();
  renderBackup();
  loadSystem();
  loadSettings();
  loadSsh();
  return () => { alive = false; offs.forEach((f) => f()); clearTimeout(timer); };
}
