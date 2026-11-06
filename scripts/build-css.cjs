// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Compiles the status page's stylesheet: only the classes the page uses, so the
// relay ships a small file and no Node.js. Run from the repo root after changing
// internal/ui/styles/app.css or the classes in the page.

const fs = require('node:fs');
const path = require('node:path');
const { compile, optimize } = require('@tailwindcss/node');
const { Scanner } = require('@tailwindcss/oxide');

const root = path.resolve(__dirname, '..');
const input = path.join(root, 'internal/ui/styles/app.css');
const output = path.join(root, 'internal/ui/web/assets/app.css');

(async () => {
  const compiler = await compile(fs.readFileSync(input, 'utf8'), {
    base: path.dirname(input),
    onDependency: () => {},
  });
  const scanner = new Scanner({ sources: compiler.sources });
  const { code } = optimize(compiler.build(scanner.scan()), { minify: true });
  fs.writeFileSync(output, code);
  console.log(`${path.relative(root, output)}: ${code.length} bytes`);
})().catch((error) => {
  console.error(error);
  process.exit(1);
});
