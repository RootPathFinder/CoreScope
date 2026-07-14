#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const assert = require('assert');
const vm = require('vm');

const src = fs.readFileSync(path.join(__dirname, 'public/managed-repeaters.js'), 'utf8');
const ctx = {
  console,
  window: {},
  localStorage: {
    _d: {},
    getItem(k) { return Object.prototype.hasOwnProperty.call(this._d, k) ? this._d[k] : null; },
    setItem(k, v) { this._d[k] = String(v); },
    removeItem(k) { delete this._d[k]; }
  },
  registerPage() {},
  escapeHtml(s) {
    return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  },
  fetch: async () => ({ ok: true, status: 200, json: async () => ({ repeaters: [] }) }),
  document: { querySelector() { return null; } },
};
ctx.window = ctx;
vm.createContext(ctx);
vm.runInContext(src, ctx);

assert.ok(ctx.window.ManagedRepeatersPage, 'ManagedRepeatersPage global exported');
assert.strictEqual(ctx.window.ManagedRepeatersPage.shortKey('aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899'), 'aabbccdd…8899');
assert.strictEqual(ctx.window.ManagedRepeatersPage.normalizeApiKeyStorageKey(), 'meshcore-api-key');

const bottomNav = fs.readFileSync(path.join(__dirname, 'public/bottom-nav.js'), 'utf8');
assert.ok(bottomNav.includes("route: 'repeaters'"), 'bottom-nav includes repeaters route');
const drawer = fs.readFileSync(path.join(__dirname, 'public/nav-drawer.js'), 'utf8');
assert.ok(drawer.includes("route: 'repeaters'"), 'nav-drawer includes repeaters route');
const html = fs.readFileSync(path.join(__dirname, 'public/index.html'), 'utf8');
assert.ok(html.includes('managed-repeaters.js'), 'index.html loads managed-repeaters.js');
assert.ok(html.includes('data-route="repeaters"'), 'index.html has repeaters nav link');

console.log('test-managed-repeaters.js: all checks passed');
