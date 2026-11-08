#!/usr/bin/env node
// Checks WCAG contrast of the UI's colour tokens (parsed from style.css) for
// the pairs that actually occur in the interface. Zero dependencies.
//
//   node tools/ui-mock/contrast-check.mjs
//
// Text pairs need >= 4.5:1 (AA); graphical/state indicators need >= 3:1.

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const css = fs.readFileSync(path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../internal/web/ui/style.css'), 'utf8');

function tokens(selectorStart) {
  const i = css.indexOf(selectorStart);
  const body = css.slice(css.indexOf('{', i) + 1, css.indexOf('}', i));
  const t = {};
  for (const m of body.matchAll(/--([a-z0-9-]+):\s*([^;]+);/g)) t[m[1]] = m[2].trim();
  return t;
}
const dark = tokens(':root {');
const light = { ...dark, ...tokens(':root[data-theme="light"]') };

function parse(c) {
  c = c.trim();
  let m = c.match(/^#([0-9a-f]{6})$/i);
  if (m) return [0, 2, 4].map((i) => parseInt(m[1].slice(i, i + 2), 16)).concat(1);
  m = c.match(/^rgba?\(([^)]+)\)$/);
  if (m) { const p = m[1].split(',').map(Number); return [p[0], p[1], p[2], p[3] ?? 1]; }
  throw new Error('cannot parse ' + c);
}
const over = (fg, bg) => { const a = fg[3]; return [0, 1, 2].map((i) => fg[i] * a + bg[i] * (1 - a)).concat(1); };
const lum = ([r, g, b]) => { const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; }; return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b); };
const ratio = (a, b) => { const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p); return (x + 0.05) / (y + 0.05); };

let failures = 0;
function check(theme, name, fgName, bgName, min, bgOver) {
  const T = theme === 'dark' ? dark : light;
  let bg = parse(T[bgName]);
  if (bgOver) bg = over(bg, parse(T[bgOver]));
  const fg = over(parse(T[fgName]), bg);
  const r = ratio(fg, bg);
  const ok = r >= min;
  if (!ok) failures++;
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${theme.padEnd(5)} ${r.toFixed(2).padStart(5)}  (need ${min})  ${name}`);
}

for (const theme of ['dark', 'light']) {
  // text
  check(theme, 'body text on page', 'text', 'bg', 4.5);
  check(theme, 'body text on card', 'text', 'surface', 4.5);
  check(theme, 'dim text on page', 'dim', 'bg', 4.5);
  check(theme, 'dim text on card', 'dim', 'surface', 4.5);
  check(theme, 'dim text on raised surface', 'dim', 'surface-2', 4.5);
    check(theme, 'link/accent text on card', 'accent', 'surface', 4.5);
  check(theme, 'accent text on page', 'accent', 'bg', 4.5);
  check(theme, 'button text on accent', 'accent-ink', 'accent', 4.5);
  check(theme, 'warning text on card', 'warn', 'surface', 4.5);
  check(theme, 'warning text on warning notice', 'warn', 'warn-soft', 4.5, 'surface');
  check(theme, 'danger text on card', 'danger', 'surface', 4.5);
  check(theme, 'danger text on danger button', 'danger', 'danger-soft', 4.5, 'surface-2');
  check(theme, 'ok text on card', 'ok', 'surface', 4.5);
  check(theme, 'text on active filter chip', 'text', 'accent-soft', 4.5, 'surface-2');
  check(theme, 'text on connected chip', 'text', 'accent-soft', 4.5, 'surface');
  check(theme, 'amber mark text on mark background', 'led-text', 'surface-2', 4.5);
  // non-text indicators
  check(theme, 'connected crosspoint dot on card', 'accent', 'surface', 3);
  check(theme, 'dormant crosspoint ring on card', 'dim', 'surface', 3);
  check(theme, 'not-connected crosspoint ring on card', 'control', 'surface', 3);
  check(theme, 'filter badge glyph on its badge', 'led-text', 'led-soft', 4.5, 'surface-2');
  check(theme, 'activity LED (lit) on card', 'led', 'surface', 3);
  check(theme, 'switch track (on) vs card', 'accent', 'surface', 3);
  check(theme, 'switch track (off) vs card', 'control', 'surface', 3);
  check(theme, 'input border vs input background', 'control', 'field', 3);
  check(theme, 'input border vs card', 'control', 'surface', 3);
  check(theme, 'dim text on input background', 'dim', 'field', 4.5);
}
console.log(failures ? `\n${failures} pair(s) below target` : '\nall pairs meet their target');
process.exit(failures ? 1 : 0);
