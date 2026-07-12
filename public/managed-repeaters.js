/* === CoreScope — managed-repeaters.js (M1 active telemetry) === */
'use strict';

(function () {
  var LS_API_KEY = 'meshcore-api-key';
  var _root = null;
  var _msgTimer = null;

  function esc(s) {
    return (typeof escapeHtml === 'function') ? escapeHtml(String(s == null ? '' : s)) : String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  }

  function apiKey() {
    try { return localStorage.getItem(LS_API_KEY) || ''; } catch (_) { return ''; }
  }

  function setApiKey(v) {
    try {
      if (v) localStorage.setItem(LS_API_KEY, v);
      else localStorage.removeItem(LS_API_KEY);
    } catch (_) { /* ignore */ }
  }

  function headers(json) {
    var h = { 'X-API-Key': apiKey() };
    if (json) h['Content-Type'] = 'application/json';
    return h;
  }

  function showMsg(text, ok) {
    var el = _root && _root.querySelector('#mr-msg');
    if (!el) return;
    el.textContent = text || '';
    el.style.color = ok ? 'var(--status-green, #45644c)' : 'var(--status-red, #b54a4a)';
    if (_msgTimer) clearTimeout(_msgTimer);
    if (text) {
      _msgTimer = setTimeout(function () { el.textContent = ''; }, 5000);
    }
  }

  function shortKey(pk) {
    if (!pk || pk.length < 12) return pk || '';
    return pk.slice(0, 8) + '…' + pk.slice(-4);
  }

  function renderList(repeaters) {
    var tbody = _root.querySelector('#mr-tbody');
    if (!tbody) return;
    if (!repeaters || !repeaters.length) {
      tbody.innerHTML = '<tr><td colspan="5" class="text-muted">No managed repeaters yet. Add one above.</td></tr>';
      return;
    }
    tbody.innerHTML = repeaters.map(function (r) {
      return '<tr data-id="' + esc(r.id) + '">'
        + '<td><code title="' + esc(r.publicKey) + '">' + esc(shortKey(r.publicKey)) + '</code></td>'
        + '<td>' + esc(r.name || '—') + '</td>'
        + '<td>' + (r.hasAdminPassword ? 'saved' : 'missing') + '</td>'
        + '<td class="text-muted">' + esc(r.updatedAt || '') + '</td>'
        + '<td><button type="button" class="btn btn-sm" data-action="delete" data-id="' + esc(r.id) + '">Remove</button></td>'
        + '</tr>';
    }).join('');
  }

  async function refresh() {
    if (!apiKey()) {
      showMsg('Enter your apiKey (from config.json) to manage repeaters.', false);
      renderList([]);
      return;
    }
    try {
      var res = await fetch('/api/managed-repeaters', { headers: headers(false) });
      var body = await res.json().catch(function () { return {}; });
      if (!res.ok) {
        showMsg((body && body.error) || ('List failed (' + res.status + ')'), false);
        return;
      }
      renderList(body.repeaters || []);
      showMsg('Loaded ' + ((body.repeaters && body.repeaters.length) || 0) + ' repeater(s).', true);
    } catch (err) {
      showMsg('List failed: ' + (err && err.message || err), false);
    }
  }

  async function onAdd(ev) {
    ev.preventDefault();
    var keyInput = _root.querySelector('#mr-apikey');
    if (keyInput) setApiKey(keyInput.value.trim());
    if (!apiKey()) {
      showMsg('apiKey required.', false);
      return;
    }
    var pk = (_root.querySelector('#mr-pubkey') || {}).value || '';
    var name = (_root.querySelector('#mr-name') || {}).value || '';
    var pass = (_root.querySelector('#mr-password') || {}).value || '';
    try {
      var res = await fetch('/api/managed-repeaters', {
        method: 'POST',
        headers: headers(true),
        body: JSON.stringify({ publicKey: pk.trim(), name: name.trim(), adminPassword: pass })
      });
      var body = await res.json().catch(function () { return {}; });
      if (!res.ok) {
        showMsg((body && body.error) || ('Add failed (' + res.status + ')'), false);
        return;
      }
      (_root.querySelector('#mr-pubkey') || {}).value = '';
      (_root.querySelector('#mr-name') || {}).value = '';
      (_root.querySelector('#mr-password') || {}).value = '';
      showMsg('Added ' + shortKey(body.publicKey) + '.', true);
      await refresh();
    } catch (err) {
      showMsg('Add failed: ' + (err && err.message || err), false);
    }
  }

  async function onDelete(id) {
    if (!id || !apiKey()) return;
    if (!confirm('Remove this managed repeater? The stored admin password will be deleted.')) return;
    try {
      var res = await fetch('/api/managed-repeaters/' + encodeURIComponent(id), {
        method: 'DELETE',
        headers: headers(false)
      });
      if (res.status !== 204 && !res.ok) {
        var body = await res.json().catch(function () { return {}; });
        showMsg((body && body.error) || ('Delete failed (' + res.status + ')'), false);
        return;
      }
      showMsg('Removed.', true);
      await refresh();
    } catch (err) {
      showMsg('Delete failed: ' + (err && err.message || err), false);
    }
  }

  function onClick(ev) {
    var btn = ev.target && ev.target.closest ? ev.target.closest('[data-action="delete"]') : null;
    if (!btn) return;
    onDelete(btn.getAttribute('data-id'));
  }

  function init(container) {
    _root = container;
    container.innerHTML =
      '<div class="managed-repeaters-page page-pad">'
      + '<h2>Managed Repeaters</h2>'
      + '<p class="text-muted">Register remote MeshCore repeaters and store their <strong>admin passwords</strong> encrypted on this server. '
      + 'Outbound polling (login / stats / telemetry) arrives in a later milestone once a local USB companion is wired.</p>'
      + '<div class="mr-card">'
      +   '<label class="mr-label">API key <input type="password" id="mr-apikey" autocomplete="off" placeholder="apiKey from config.json"></label>'
      +   '<p class="text-muted mr-hint">Stored only in this browser (localStorage). Required for all vault operations.</p>'
      + '</div>'
      + '<form id="mr-add-form" class="mr-card">'
      +   '<h3>Add repeater</h3>'
      +   '<label class="mr-label">Public key <input id="mr-pubkey" required spellcheck="false" placeholder="64-char hex pubkey"></label>'
      +   '<label class="mr-label">Display name <input id="mr-name" maxlength="128" placeholder="optional"></label>'
      +   '<label class="mr-label">Admin password <input id="mr-password" type="password" required autocomplete="new-password"></label>'
      +   '<button type="submit" class="btn">Save encrypted</button>'
      + '</form>'
      + '<p id="mr-msg" class="mr-msg" role="status" aria-live="polite"></p>'
      + '<div class="mr-card">'
      +   '<div class="mr-toolbar"><h3>Registered</h3><button type="button" class="btn btn-sm" id="mr-refresh">Refresh</button></div>'
      +   '<table class="analytics-table" id="mr-table">'
      +     '<thead><tr><th>Pubkey</th><th>Name</th><th>Password</th><th>Updated</th><th></th></tr></thead>'
      +     '<tbody id="mr-tbody"><tr><td colspan="5" class="text-muted">Loading…</td></tr></tbody>'
      +   '</table>'
      + '</div>'
      + '</div>';

    var keyInput = container.querySelector('#mr-apikey');
    if (keyInput) keyInput.value = apiKey();
    container.querySelector('#mr-add-form').addEventListener('submit', onAdd);
    container.querySelector('#mr-refresh').addEventListener('click', function () {
      if (keyInput) setApiKey(keyInput.value.trim());
      refresh();
    });
    container.addEventListener('click', onClick);
    refresh();
  }

  function destroy() {
    if (_msgTimer) clearTimeout(_msgTimer);
    _msgTimer = null;
    _root = null;
  }

  // Exposed for unit tests.
  window.ManagedRepeatersPage = {
    shortKey: shortKey,
    normalizeApiKeyStorageKey: function () { return LS_API_KEY; }
  };

  registerPage('repeaters', { init: init, destroy: destroy });
})();
