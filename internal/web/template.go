package web

// shell is the entire dashboard: a static application shell with no ledger
// content baked in. It fetches /api/state with the admin token held in
// localStorage, so refreshing no longer discards the token the operator typed.
//
// Every value that originates outside Watchtower — diagnosis prose, evidence
// paths, session identifiers, provider URLs — is written through textContent or
// a scheme-checked href. The page never assigns untrusted text to innerHTML,
// because that text is authored by a coding agent reading untrusted CI output.
const shell = `<!doctype html>
<html lang="en" data-demo="false">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>AO Watchtower</title>
<style>
:root{--bg:#0e1319;--panel:#161d26;--inset:#0d141c;--line:#2a3441;--text:#e6edf5;--muted:#8b98a8;--accent:#5aa9ff;--ok:#3fb950;--warn:#d29922;--bad:#f85149;--radius:10px}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);font:14px/1.5 ui-sans-serif,system-ui,-apple-system,Segoe UI,Roboto,sans-serif}
header{padding:1.25rem 1.5rem;border-bottom:1px solid var(--line);display:flex;flex-wrap:wrap;gap:1rem;align-items:center}
h1{font-size:1.05rem;margin:0;letter-spacing:.02em}
h1 span{color:var(--muted);font-weight:400}
main{padding:1.5rem;max-width:1180px;margin:0 auto}
.bar{display:flex;flex-wrap:wrap;gap:.5rem;align-items:center;margin-left:auto}
input,select,button{font:inherit;padding:.4rem .6rem;border-radius:8px;border:1px solid var(--line);background:var(--panel);color:var(--text)}
button{cursor:pointer}
button:hover{border-color:var(--accent)}
button.primary{background:var(--accent);border-color:var(--accent);color:#06121f;font-weight:600}
button:disabled{opacity:.4;cursor:not-allowed}
.stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:.75rem;margin-bottom:1.5rem}
.stat{background:var(--panel);border:1px solid var(--line);border-radius:var(--radius);padding:.75rem .9rem}
.stat b{display:block;font-size:1.5rem;font-weight:600;line-height:1.2}
.stat span{color:var(--muted);font-size:.78rem;text-transform:uppercase;letter-spacing:.05em}
.card{background:var(--panel);border:1px solid var(--line);border-radius:var(--radius);padding:1rem;margin-bottom:.85rem}
.card header{padding:0;border:0;gap:.6rem;align-items:baseline}
.who{font-weight:600}
.sha{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.8rem;color:var(--muted)}
.pill{font-size:.72rem;text-transform:uppercase;letter-spacing:.06em;padding:.18rem .5rem;border-radius:999px;border:1px solid var(--line);white-space:nowrap}
.pill.fixed{border-color:var(--ok);color:var(--ok)}
.pill.verifying,.pill.investigating{border-color:var(--accent);color:var(--accent)}
.pill.awaiting_approval,.pill.stalled,.pill.unverified,.pill.owned_elsewhere{border-color:var(--warn);color:var(--warn)}
.pill.fix_did_not_work,.pill.spawn_failed,.pill.dispatch_failed,.pill.held_by_kill_switch{border-color:var(--bad);color:var(--bad)}
.diag{margin-top:.75rem;padding:.75rem;background:var(--inset);border:1px solid var(--line);border-radius:8px}
.diag p{margin:.2rem 0 .6rem}
.diag table{width:100%;border-collapse:collapse;font-size:.85rem}
.diag td{padding:.25rem .5rem;border-top:1px solid var(--line);color:var(--muted)}
.diag td:first-child{color:var(--text);font-family:ui-monospace,Menlo,monospace}
.meta{color:var(--muted);font-size:.8rem;margin-top:.6rem;display:flex;flex-wrap:wrap;gap:.9rem}
.actions{margin-top:.8rem;display:flex;gap:.5rem;flex-wrap:wrap}
a{color:var(--accent)}
.banner{padding:.7rem 1rem;border-radius:var(--radius);margin-bottom:1rem;border:1px solid var(--warn);color:var(--warn)}
.banner.demo{border-color:var(--bad);color:var(--bad)}
#note{color:var(--muted);font-size:.82rem}
.empty{color:var(--muted);padding:2rem;text-align:center;border:1px dashed var(--line);border-radius:var(--radius)}
</style>
</head>
<body>
<header>
  <h1>AO Watchtower <span>supervised CI repair</span></h1>
  <div class="bar">
    <input id="token" type="password" placeholder="Admin token" autocomplete="off" size="22">
    <input id="actor" maxlength="128" placeholder="Actor" size="14">
    <select id="filter">
      <option value="">All events</option>
      <option value="open">Needs attention</option>
      <option value="awaiting_approval">Awaiting approval</option>
      <option value="investigating">Investigating</option>
      <option value="fixed">Fixed</option>
    </select>
    <button id="kill">Automation</button>
    <span id="note"></span>
  </div>
</header>
<main>
  <div id="banners"></div>
  <div class="stats" id="stats"></div>
  <div id="rows"></div>
</main>
<script>
(function () {
  var TOKEN_KEY = 'ao-watchtower-admin-token';
  var ACTOR_KEY = 'ao-watchtower-actor';
  var tokenInput = document.getElementById('token');
  var actorInput = document.getElementById('actor');
  var filterInput = document.getElementById('filter');
  var note = document.getElementById('note');
  var demo = document.documentElement.getAttribute('data-demo') === 'true';

  tokenInput.value = localStorage.getItem(TOKEN_KEY) || (demo ? 'demo-admin-token' : '');
  actorInput.value = localStorage.getItem(ACTOR_KEY) || 'local-user';
  tokenInput.addEventListener('change', function () { localStorage.setItem(TOKEN_KEY, tokenInput.value); refresh(); });
  actorInput.addEventListener('change', function () { localStorage.setItem(ACTOR_KEY, actorInput.value); });
  filterInput.addEventListener('change', render);

  var state = null;
  var NEEDS_ATTENTION = ['awaiting_approval', 'stalled', 'spawn_failed', 'dispatch_failed', 'fix_did_not_work', 'unverified', 'owned_elsewhere'];

  function element(tag, className, text) {
    var node = document.createElement(tag);
    if (className) { node.className = className; }
    if (text !== undefined && text !== null) { node.textContent = String(text); }
    return node;
  }

  // Provider URLs arrive from GitHub payloads. Only http(s) links are rendered;
  // anything else is shown as inert text.
  function link(url, text) {
    if (typeof url !== 'string' || !/^https?:\/\//i.test(url)) { return element('span', null, text); }
    var anchor = element('a', null, text);
    anchor.href = url;
    anchor.rel = 'noreferrer noopener';
    anchor.target = '_blank';
    return anchor;
  }

  function authHeaders(json) {
    var headers = { Authorization: 'Bearer ' + tokenInput.value };
    if (json) { headers['Content-Type'] = 'application/json'; }
    return headers;
  }

  function post(path, body) {
    return fetch(path, { method: 'POST', headers: authHeaders(!!body), body: body ? JSON.stringify(body) : undefined })
      .then(function (response) {
        note.textContent = response.ok ? 'Saved' : 'Request failed (' + response.status + ')';
        return refresh();
      })
      .catch(function () { note.textContent = 'Request failed'; });
  }

  function refresh() {
    if (!tokenInput.value) { note.textContent = 'Enter the admin token printed at startup'; return Promise.resolve(); }
    return fetch('/api/state', { headers: authHeaders(false) })
      .then(function (response) {
        if (response.status === 401) { note.textContent = 'Admin token rejected'; return null; }
        if (!response.ok) { note.textContent = 'State unavailable'; return null; }
        note.textContent = '';
        return response.json();
      })
      .then(function (payload) { if (payload) { state = payload; render(); } })
      .catch(function () { note.textContent = 'Watchtower unreachable'; });
  }

  function renderStats() {
    var container = document.getElementById('stats');
    container.textContent = '';
    if (!state) { return; }
    var stats = state.stats;
    var success = (stats.verifiedGreen + stats.stillFailing) > 0 ? Math.round(stats.repairSuccessRate * 100) + '%' : '—';
    [
      ['Failures seen', stats.triggers],
      ['Investigated', stats.spawned],
      ['Awaiting approval', Math.max(stats.validDiagnoses - stats.approvals, 0)],
      ['Fixes dispatched', stats.dispatched],
      ['Verified green', stats.verifiedGreen],
      ['Repair success', success],
      ['Median time to green', stats.medianTimeToGreen || '—']
    ].forEach(function (pair) {
      var card = element('div', 'stat');
      card.appendChild(element('b', null, pair[1]));
      card.appendChild(element('span', null, pair[0]));
      container.appendChild(card);
    });
  }

  function renderBanners() {
    var container = document.getElementById('banners');
    container.textContent = '';
    if (!state) { return; }
    if (state.demo) {
      container.appendChild(element('div', 'banner demo', 'DEMO MODE — fake AO boundary, never production automation. Admin token: demo-admin-token'));
    }
    if (state.automationDisabled) {
      container.appendChild(element('div', 'banner', 'KILL SWITCH ENABLED — no new investigations or fixes will be dispatched.'));
    }
    document.getElementById('kill').textContent = state.automationDisabled ? 'Enable automation' : 'Disable automation';
  }

  function renderDiagnosis(row) {
    var box = element('div', 'diag');
    var head = element('div');
    head.appendChild(element('span', 'pill', row.diagnosis.category));
    head.appendChild(document.createTextNode(' '));
    head.appendChild(element('span', 'pill', Math.round(row.diagnosis.confidence * 100) + '% confidence'));
    head.appendChild(document.createTextNode(' '));
    head.appendChild(element('span', 'pill', row.diagnosis.recommendedAction));
    box.appendChild(head);
    box.appendChild(element('p', null, row.diagnosis.summary));
    var evidence = row.diagnosis.evidence || [];
    if (evidence.length) {
      var table = element('table');
      evidence.forEach(function (item) {
        var line = element('tr');
        line.appendChild(element('td', null, (item.file || '(no file)') + (item.line ? ':' + item.line : '')));
        line.appendChild(element('td', null, item.check || ''));
        table.appendChild(line);
      });
      box.appendChild(table);
    }
    return box;
  }

  function renderActions(row) {
    var actions = element('div', 'actions');
    if (row.canApprove) {
      var approve = element('button', null, 'Approve');
      approve.onclick = function () { post('/api/triggers?action=approve&trigger=' + encodeURIComponent(row.triggerKey), { actor: actorInput.value }); };
      actions.appendChild(approve);
    }
    if (row.canFix) {
      var fix = element('button', 'primary', 'Fix with AO');
      fix.onclick = function () { post('/api/triggers?action=fix&trigger=' + encodeURIComponent(row.triggerKey)); };
      actions.appendChild(fix);
    }
    if (row.canRetry) {
      var retry = element('button', null, 'Retry dispatch');
      retry.onclick = function () { post('/api/triggers?action=retry&trigger=' + encodeURIComponent(row.triggerKey), { actor: actorInput.value }); };
      actions.appendChild(retry);
    }
    return actions;
  }

  function render() {
    renderBanners();
    renderStats();
    var container = document.getElementById('rows');
    container.textContent = '';
    if (!state) { return; }
    var wanted = filterInput.value;
    var rows = state.rows.filter(function (row) {
      if (!wanted) { return true; }
      if (wanted === 'open') { return NEEDS_ATTENTION.indexOf(row.status) !== -1; }
      return row.status === wanted;
    });
    if (!rows.length) {
      container.appendChild(element('div', 'empty', 'No events match this filter yet.'));
      return;
    }
    rows.forEach(function (row) {
      var card = element('div', 'card');
      var head = element('header');
      head.appendChild(element('span', 'who', row.repository + ' #' + row.pullNumber));
      head.appendChild(element('span', 'sha', row.headSHA.slice(0, 12)));
      head.appendChild(element('span', 'pill ' + row.status, row.status.replace(/_/g, ' ')));
      card.appendChild(head);
      if (row.diagnosis) { card.appendChild(renderDiagnosis(row)); }
      var meta = element('div', 'meta');
      meta.appendChild(element('span', null, new Date(row.createdAt).toLocaleString()));
      if (row.conclusion) { meta.appendChild(element('span', null, 'CI ' + row.conclusion)); }
      if (row.detailsURL) { meta.appendChild(link(row.detailsURL, 'CI run')); }
      if (row.sessionId) { meta.appendChild(element('span', null, 'AO session ' + row.sessionId)); }
      if (row.approval) { meta.appendChild(element('span', null, 'approved by ' + row.approval)); }
      if (row.sendDetail) { meta.appendChild(element('span', null, row.sendDetail)); }
      card.appendChild(meta);
      card.appendChild(renderActions(row));
      container.appendChild(card);
    });
  }

  document.getElementById('kill').onclick = function () {
    if (!state) { return; }
    post('/api/automation', { disabled: !state.automationDisabled, actor: actorInput.value });
  };

  refresh();
  setInterval(refresh, 5000);
})();
</script>
</body>
</html>`
