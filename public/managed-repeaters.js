/* === CoreScope — managed-repeaters.js (M1 vault + M2 poll status) === */
'use strict';

(function () {
  var LS_API_KEY = 'meshcore-api-key';
  var _root = null;
  var _msgTimer = null;
  var _pollTimer = null;

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

  function fmtUptime(secs) {
    secs = Number(secs) || 0;
    if (secs < 60) return secs + 's';
    if (secs < 3600) return Math.floor(secs / 60) + 'm';
    if (secs < 86400) return (secs / 3600).toFixed(1) + 'h';
    return (secs / 86400).toFixed(1) + 'd';
  }

  function renderCompanion(companion, statusAt) {
    var el = _root.querySelector('#mr-companion');
    if (!el) return;
    if (!companion) {
      el.innerHTML = '<p class="text-muted">No companion-poller status yet. Deploy the poller service with <code>COMPANION_SERIAL=/dev/ttyACM1</code>.</p>';
      return;
    }
    var ok = !!companion.ok;
    el.innerHTML =
      '<div class="mr-companion-row">'
      + '<span class="mr-pill ' + (ok ? 'mr-pill-ok' : 'mr-pill-bad') + '">' + (ok ? 'Companion up' : 'Companion down') + '</span>'
      + '<span class="text-muted">' + esc(companion.port || '') + (companion.baud ? (' @ ' + companion.baud) : '') + '</span>'
      + (statusAt ? '<span class="text-muted">status ' + esc(statusAt) + '</span>' : '')
      + (companion.lastError ? '<span class="mr-err">' + esc(companion.lastError) + '</span>' : '')
      + '</div>';
  }

  function renderCards(repeaters) {
    var host = _root.querySelector('#mr-cards');
    if (!host) return;
    if (!repeaters || !repeaters.length) {
      host.innerHTML = '<p class="text-muted">No managed repeaters yet.</p>';
      return;
    }
    host.innerHTML = repeaters.map(function (r) {
      var poll = r.poll || null;
      var stats = poll && poll.stats ? poll.stats : null;
      var statusClass = !poll ? 'mr-pill-muted' : (poll.ok ? 'mr-pill-ok' : 'mr-pill-bad');
      var statusLabel = !poll ? 'Never polled' : (poll.ok ? 'Online' : 'Poll failed');
      var body = '';
      if (stats) {
        body =
          '<div class="mr-stat-grid">'
          + '<div><span class="mr-stat-label">Battery</span><span class="mr-stat-val">' + esc(String(stats.batteryMv)) + ' mV</span></div>'
          + '<div><span class="mr-stat-label">Uptime</span><span class="mr-stat-val">' + esc(fmtUptime(stats.uptimeSecs)) + '</span></div>'
          + '<div><span class="mr-stat-label">Noise</span><span class="mr-stat-val">' + esc(String(stats.noiseFloor)) + ' dBm</span></div>'
          + '<div><span class="mr-stat-label">Last SNR</span><span class="mr-stat-val">' + esc(Number(stats.lastSnr).toFixed(1)) + ' dB</span></div>'
          + '<div><span class="mr-stat-label">RX / TX</span><span class="mr-stat-val">' + esc(String(stats.packetsRecv)) + ' / ' + esc(String(stats.packetsSent)) + '</span></div>'
          + '<div><span class="mr-stat-label">Queue</span><span class="mr-stat-val">' + esc(String(stats.txQueueLen)) + '</span></div>'
          + '</div>';
      } else if (poll && poll.error) {
        body = '<p class="mr-err">' + esc(poll.error) + '</p>';
      } else {
        body = '<p class="text-muted">Waiting for companion-poller…</p>';
      }
      return '<article class="mr-monitor-card" data-id="' + esc(r.id) + '">'
        + '<header class="mr-monitor-head">'
        +   '<div><strong>' + esc(r.name || shortKey(r.publicKey)) + '</strong>'
        +   '<div class="text-muted"><code title="' + esc(r.publicKey) + '">' + esc(shortKey(r.publicKey)) + '</code></div></div>'
        +   '<span class="mr-pill ' + statusClass + '">' + statusLabel + '</span>'
        + '</header>'
        + body
        + '<footer class="mr-monitor-foot text-muted">'
        +   (poll && poll.polledAt ? ('Last poll ' + esc(poll.polledAt)) : 'Not polled yet')
        +   (poll && poll.isAdmin ? ' · admin' : '')
        +   ' · <button type="button" class="btn btn-sm" data-action="delete" data-id="' + esc(r.id) + '">Remove</button>'
        + '</footer>'
        + '</article>';
    }).join('');
  }

  function renderList(repeaters) {
    var tbody = _root.querySelector('#mr-tbody');
    if (!tbody) return;
    if (!repeaters || !repeaters.length) {
      tbody.innerHTML = '<tr><td colspan="5" class="text-muted">No managed repeaters yet. Add one above.</td></tr>';
      return;
    }
    tbody.innerHTML = repeaters.map(function (r) {
      var poll = r.poll;
      var pollCell = '—';
      if (poll && poll.ok && poll.stats) pollCell = poll.stats.batteryMv + ' mV';
      else if (poll && poll.error) pollCell = 'err';
      else if (poll) pollCell = 'fail';
      return '<tr data-id="' + esc(r.id) + '">'
        + '<td><code title="' + esc(r.publicKey) + '">' + esc(shortKey(r.publicKey)) + '</code></td>'
        + '<td>' + esc(r.name || '—') + '</td>'
        + '<td>' + (r.hasAdminPassword ? 'saved' : 'missing') + '</td>'
        + '<td>' + esc(pollCell) + '</td>'
        + '<td><button type="button" class="btn btn-sm" data-action="delete" data-id="' + esc(r.id) + '">Remove</button></td>'
        + '</tr>';
    }).join('');
  }

  async function refresh(silent) {
    if (!apiKey()) {
      if (!silent) showMsg('Enter your apiKey (from config.json) to manage repeaters.', false);
      renderList([]);
      renderCards([]);
      return;
    }
    try {
      var res = await fetch('/api/managed-repeaters', { headers: headers(false) });
      var body = await res.json().catch(function () { return {}; });
      if (!res.ok) {
        if (!silent) showMsg((body && body.error) || ('List failed (' + res.status + ')'), false);
        return;
      }
      var list = body.repeaters || [];
      renderList(list);
      renderCards(list);
      renderCompanion(body.companion, body.statusUpdatedAt);
      if (!silent) showMsg('Loaded ' + list.length + ' repeater(s).', true);
    } catch (err) {
      if (!silent) showMsg('List failed: ' + (err && err.message || err), false);
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
      showMsg('Added ' + shortKey(body.publicKey) + '. Poller will pick it up on the next cycle.', true);
      await refresh(true);
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
      await refresh(true);
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
      + '<p class="text-muted">Encrypted admin passwords + live status from the local USB companion poller '
      + '(login → status). Companion must already know each repeater as a contact (heard advert).</p>'
      + '<div class="mr-card" id="mr-companion"><p class="text-muted">Checking companion…</p></div>'
      + '<div class="mr-card">'
      +   '<label class="mr-label">API key <input type="password" id="mr-apikey" autocomplete="off" placeholder="apiKey from config.json"></label>'
      +   '<p class="text-muted mr-hint">Stored only in this browser (localStorage). Required for vault operations.</p>'
      + '</div>'
      + '<form id="mr-add-form" class="mr-card">'
      +   '<h3>Add repeater</h3>'
      +   '<label class="mr-label">Public key <input id="mr-pubkey" required spellcheck="false" placeholder="64-char hex pubkey"></label>'
      +   '<label class="mr-label">Display name <input id="mr-name" maxlength="128" placeholder="optional"></label>'
      +   '<label class="mr-label">Admin password <input id="mr-password" type="password" required autocomplete="new-password" maxlength="15"></label>'
      +   '<p class="text-muted mr-hint">MeshCore companion login passwords are max 15 characters.</p>'
      +   '<button type="submit" class="btn">Save encrypted</button>'
      + '</form>'
      + '<p id="mr-msg" class="mr-msg" role="status" aria-live="polite"></p>'
      + '<div class="mr-toolbar"><h3>Monitoring</h3><button type="button" class="btn btn-sm" id="mr-refresh">Refresh</button></div>'
      + '<div id="mr-cards" class="mr-cards"></div>'
      + '<div class="mr-card">'
      +   '<h3>Registry</h3>'
      +   '<table class="analytics-table" id="mr-table">'
      +     '<thead><tr><th>Pubkey</th><th>Name</th><th>Password</th><th>Battery</th><th></th></tr></thead>'
      +     '<tbody id="mr-tbody"><tr><td colspan="5" class="text-muted">Loading…</td></tr></tbody>'
      +   '</table>'
      + '</div>'
      + '</div>';

    var keyInput = container.querySelector('#mr-apikey');
    if (keyInput) keyInput.value = apiKey();
    container.querySelector('#mr-add-form').addEventListener('submit', onAdd);
    container.querySelector('#mr-refresh').addEventListener('click', function () {
      if (keyInput) setApiKey(keyInput.value.trim());
      refresh(false);
    });
    container.addEventListener('click', onClick);
    refresh(false);
    _pollTimer = setInterval(function () { refresh(true); }, 30000);
  }

  function destroy() {
    if (_msgTimer) clearTimeout(_msgTimer);
    if (_pollTimer) clearInterval(_pollTimer);
    _msgTimer = null;
    _pollTimer = null;
    _root = null;
  }

  /**
   * Prompt for API key (if needed) + admin password, then POST to the vault.
   * Used from node detail / My Repeaters "Add to monitoring" buttons.
   * @returns {Promise<{ok:boolean, error?:string, already?:boolean}>}
   */
  async function promptAddMonitoring(publicKey, name) {
    var pk = String(publicKey || '').trim().toLowerCase();
    if (!/^[0-9a-f]{64}$/.test(pk)) {
      return { ok: false, error: 'Invalid public key' };
    }
    var key = apiKey();
    if (!key) {
      key = window.prompt('Enter CoreScope API key (from config.json / CORESCOPE_API_KEY):', '') || '';
      key = key.trim();
      if (!key) return { ok: false, error: 'API key required' };
      setApiKey(key);
    }
    var pass = window.prompt(
      'Admin password for ' + (name || shortKey(pk)) + ' (max 15 characters):',
      ''
    );
    if (pass == null) return { ok: false, error: 'Cancelled' };
    pass = String(pass);
    if (!pass) return { ok: false, error: 'Admin password required' };
    if (pass.length > 15) return { ok: false, error: 'Admin password max 15 characters (companion protocol limit)' };

    try {
      var res = await fetch('/api/managed-repeaters', {
        method: 'POST',
        headers: headers(true),
        body: JSON.stringify({
          publicKey: pk,
          name: (name && String(name).trim()) || '',
          adminPassword: pass
        })
      });
      var body = await res.json().catch(function () { return {}; });
      if (res.status === 409) {
        return { ok: false, already: true, error: (body && body.error) || 'Already in monitoring' };
      }
      if (!res.ok) {
        return { ok: false, error: (body && body.error) || ('Add failed (' + res.status + ')') };
      }
      return { ok: true, id: body.id, publicKey: body.publicKey || pk };
    } catch (err) {
      return { ok: false, error: String(err && err.message || err) };
    }
  }

  async function addMonitoringClick(publicKey, name) {
    var result = await promptAddMonitoring(publicKey, name);
    if (result.ok) {
      if (window.confirm('Added to monitoring. Open the Repeaters page now?')) {
        location.hash = '#/repeaters';
      }
      return result;
    }
    if (result.already) {
      if (window.confirm('Already in monitoring. Open the Repeaters page?')) {
        location.hash = '#/repeaters';
      }
      return result;
    }
    if (result.error && result.error !== 'Cancelled') {
      window.alert(result.error);
    }
    return result;
  }

  window.ManagedRepeatersPage = {
    shortKey: shortKey,
    fmtUptime: fmtUptime,
    normalizeApiKeyStorageKey: function () { return LS_API_KEY; },
    promptAddMonitoring: promptAddMonitoring,
    addMonitoringClick: addMonitoringClick
  };

  registerPage('repeaters', { init: init, destroy: destroy });
})();
