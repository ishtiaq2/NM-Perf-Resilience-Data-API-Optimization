#!/usr/bin/env node
'use strict';
/** Re-render an HTML report from a saved compare-*.json:  node bench/render-report.js bench/results/compare-XYZ.json */
const fs = require('node:fs');
const { renderReport } = require('./report-html');
const file = process.argv[2];
if (!file) { console.error('usage: node bench/render-report.js <compare.json> [out.html]'); process.exit(2); }
const runs = JSON.parse(fs.readFileSync(file, 'utf8'));
const out = process.argv[3] || file.replace(/\.json$/, '.html');
fs.writeFileSync(out, renderReport(runs, { subtitle: process.env.REPORT_SUBTITLE || '', footnote: process.env.REPORT_FOOTNOTE || '' }));
console.log('wrote ' + out);
