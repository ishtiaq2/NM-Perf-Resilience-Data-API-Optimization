#!/usr/bin/env node
'use strict';
/**
 * Guard for old device runtimes: every file under src/ must parse as
 * ECMAScript 2019 (the level of Node 12). Catches ?. ?? ??= class fields etc.
 * before they reach a customer device with an old Node version.
 *   npm run check:es2019
 */
const fs = require('node:fs');
const path = require('node:path');
const acorn = require('acorn');

const root = path.join(__dirname, '..', 'src');
let failed = 0;
let files = 0;
(function walk(dir) {
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) walk(p);
    else if (e.name.endsWith('.js')) {
      files++;
      try {
        acorn.parse(fs.readFileSync(p, 'utf8'), { ecmaVersion: 2019, sourceType: 'script', allowHashBang: true });
      } catch (err) {
        failed++;
        console.error(`${path.relative(process.cwd(), p)}: ${err.message}`);
      }
    }
  }
})(root);
console.log(failed ? `${failed} of ${files} files use syntax newer than ES2019` : `all ${files} runtime files are ES2019 (Node 12+) compatible`);
process.exit(failed ? 1 : 0);
