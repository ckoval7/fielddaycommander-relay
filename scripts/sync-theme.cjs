// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Copies Field Day Commander's theme colors and the rules that apply them, so the
// relay's page can't drift from the main app by hand. Pass the app's stylesheet if
// it isn't in the usual place, and --check to report a difference without writing.
//
//   node scripts/sync-theme.cjs [--check] [path/to/resources/css/app.css]

const fs = require('node:fs');
const path = require('node:path');

const root = path.resolve(__dirname, '..');
const output = path.join(root, 'internal/ui/styles/theme.css');
const args = process.argv.slice(2);
const check = args.includes('--check');
const source = args.find((arg) => !arg.startsWith('--')) ||
  path.resolve(root, '../fielddaycommander/resources/css/app.css');

function between(text, start, end) {
  const from = text.indexOf(start);
  const to = text.indexOf(end, from);
  if (from < 0 || to < 0) {
    throw new Error(`Can't find "${start}" through "${end}" in ${source}. Has the app's stylesheet changed?`);
  }
  return text.slice(from, to).trim();
}

const app = fs.readFileSync(source, 'utf8');
const theme = `/*
 * SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
 * SPDX-License-Identifier: GPL-3.0-or-later
 *
 * Generated from Field Day Commander's resources/css/app.css by
 * scripts/sync-theme.cjs. Change the colors there, not here.
 */

${between(app, '@plugin "daisyui/theme"', '/* maryUI */')}

${between(app, '/* Solid fills:', '/* Checkboxes without')}
`;

if (check) {
  const current = fs.existsSync(output) ? fs.readFileSync(output, 'utf8') : '';
  if (current !== theme) {
    console.error('The theme differs from the main app. Run scripts/sync-theme.cjs, then scripts/build-css.cjs.');
    process.exit(1);
  }
  console.log('The theme matches the main app.');
} else {
  fs.writeFileSync(output, theme);
  console.log(`${path.relative(root, output)}: copied from ${source}`);
}
