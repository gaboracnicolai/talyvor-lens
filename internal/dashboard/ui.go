package dashboard

// dashboardHTML is the single-file Talyvor Lens dashboard. {{VERSION}}
// is replaced at construction time. The page fetches /v1/api/* via XHR
// every 30 seconds; data calls degrade gracefully when the API key
// isn't configured (the dashboard itself is unauthenticated).
const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Talyvor Lens · AI token intelligence</title>
  <meta name="application-name" content="TALYVOR LENS">
  <meta property="og:title" content="TALYVOR LENS">
  <meta property="og:description" content="AI token intelligence — real-time view of LLM spend, cache, and routing.">
  <meta name="theme-color" content="#0c0e12">
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
  <link href="https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500;600&family=IBM+Plex+Sans:wght@400;500;600;700&display=swap" rel="stylesheet">
  <style>
    :root {
      --bg: #0c0e12;
      --panel: #14171e;
      --border: #1c1f26;
      --text: #d4d8e2;
      --secondary: #8892a4;
      --accent: #f0a030;
      --good: #5ac17d;
      --warn: #f0a030;
      --bad: #e35d6a;
    }
    * { box-sizing: border-box; }
    html, body { margin: 0; padding: 0; }
    body {
      font-family: 'IBM Plex Sans', -apple-system, BlinkMacSystemFont, sans-serif;
      background: var(--bg);
      color: var(--text);
      min-height: 100vh;
      font-size: 14px;
      line-height: 1.5;
    }
    .mono { font-family: 'IBM Plex Mono', 'SF Mono', monospace; }
    .accent { color: var(--accent); }
    .muted { color: var(--secondary); }

    header {
      padding: 22px 32px;
      border-bottom: 1px solid var(--border);
      display: flex;
      align-items: baseline;
      gap: 16px;
      flex-wrap: wrap;
    }
    header h1 {
      margin: 0;
      font-size: 22px;
      font-weight: 600;
      letter-spacing: 0.06em;
    }
    .badge {
      font-family: 'IBM Plex Mono', monospace;
      font-size: 12px;
      padding: 3px 9px;
      background: var(--border);
      border-radius: 4px;
      color: var(--secondary);
    }
    .tagline { color: var(--secondary); font-size: 14px; }
    .updated {
      margin-left: auto;
      color: var(--secondary);
      font-size: 13px;
      font-family: 'IBM Plex Mono', monospace;
    }

    main {
      padding: 32px;
      max-width: 1400px;
      margin: 0 auto;
    }
    @media (max-width: 700px) { main { padding: 20px; } }

    .alert {
      display: none;
      background: rgba(240, 160, 48, 0.08);
      border: 1px solid rgba(240, 160, 48, 0.5);
      color: var(--warn);
      padding: 12px 16px;
      border-radius: 6px;
      margin-bottom: 24px;
      font-size: 14px;
    }
    .alert.visible { display: block; }

    section { margin-bottom: 36px; }
    section h2 {
      font-size: 12px;
      text-transform: uppercase;
      letter-spacing: 0.12em;
      color: var(--secondary);
      margin: 0 0 14px 0;
      font-weight: 500;
    }

    .cards {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
      gap: 14px;
    }
    .card {
      background: var(--panel);
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 18px 20px;
    }
    .card .label {
      font-size: 11px;
      color: var(--secondary);
      text-transform: uppercase;
      letter-spacing: 0.1em;
    }
    .card .value {
      font-family: 'IBM Plex Mono', monospace;
      font-size: 28px;
      font-weight: 500;
      margin-top: 8px;
      color: var(--text);
      transition: color 0.2s ease;
    }
    .card .sub {
      margin-top: 6px;
      font-size: 12px;
      color: var(--secondary);
    }
    .value.good { color: var(--good); }
    .value.warn { color: var(--warn); }
    .value.bad  { color: var(--bad);  }

    table {
      width: 100%;
      border-collapse: collapse;
      font-size: 13px;
      background: var(--panel);
      border: 1px solid var(--border);
      border-radius: 8px;
      overflow: hidden;
    }
    th, td {
      padding: 10px 14px;
      text-align: left;
      border-bottom: 1px solid var(--border);
    }
    tbody tr:last-child td { border-bottom: 0; }
    th {
      text-transform: uppercase;
      font-size: 10px;
      letter-spacing: 0.1em;
      color: var(--secondary);
      font-weight: 500;
      background: rgba(28, 31, 38, 0.4);
    }
    td.num { text-align: right; }
    .bar {
      display: inline-block;
      height: 6px;
      background: var(--accent);
      border-radius: 3px;
      vertical-align: middle;
      margin-left: 8px;
    }

    .pill {
      display: inline-block;
      padding: 4px 10px;
      border-radius: 12px;
      font-size: 12px;
      font-family: 'IBM Plex Mono', monospace;
    }
    .pill.good { background: rgba(90, 193, 125, 0.12);  color: var(--good); }
    .pill.bad  { background: rgba(227, 93, 106, 0.12);  color: var(--bad); }
    .pill.warn { background: rgba(240, 160, 48, 0.12);  color: var(--warn); }

    .twobox {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 14px;
      margin-bottom: 18px;
    }
    @media (max-width: 600px) { .twobox { grid-template-columns: 1fr; } }

    .skeleton { color: var(--secondary); font-style: italic; }

    footer {
      padding: 28px 32px;
      text-align: center;
      color: var(--secondary);
      font-size: 13px;
      border-top: 1px solid var(--border);
    }
    footer a { color: var(--accent); text-decoration: none; }
    footer a:hover { text-decoration: underline; }
  </style>
</head>
<body>
  <header>
    <h1>TALYVOR <span class="accent">LENS</span></h1>
    <span class="badge mono">v{{VERSION}}</span>
    <span class="tagline">AI token intelligence</span>
    <span class="updated mono" id="updated">—</span>
  </header>

  <main>
    <div class="alert" id="api-alert">
      ⚠ Unable to reach Lens API. Configure your API key (header <code class="mono">Authorization: Bearer &lt;key&gt;</code>) to view live data.
    </div>

    <section>
      <h2>Summary</h2>
      <div class="cards">
        <div class="card">
          <div class="label">Total Spend</div>
          <div class="value mono" id="spend-total">—</div>
          <div class="sub">last 30 days</div>
        </div>
        <div class="card">
          <div class="label">Cache Hit Rate</div>
          <div class="value mono" id="spend-hitrate">—</div>
          <div class="sub" id="spend-hitrate-sub">—</div>
        </div>
        <div class="card">
          <div class="label">Total Requests</div>
          <div class="value mono" id="spend-requests">—</div>
          <div class="sub" id="spend-requests-sub">—</div>
        </div>
        <div class="card">
          <div class="label">Avg Cost / Request</div>
          <div class="value mono" id="spend-avg">—</div>
          <div class="sub" id="spend-avg-sub">—</div>
        </div>
      </div>
    </section>

    <section>
      <h2>Spend by Model</h2>
      <table>
        <thead>
          <tr>
            <th>Model</th>
            <th class="num">Requests</th>
            <th class="num">Input</th>
            <th class="num">Output</th>
            <th class="num">Cost</th>
            <th class="num">% of total</th>
          </tr>
        </thead>
        <tbody id="models-body">
          <tr><td colspan="6" class="skeleton">Loading…</td></tr>
        </tbody>
      </table>
    </section>

    <section>
      <h2>Cache Performance</h2>
      <div class="twobox">
        <div class="card">
          <div class="label">Hit Rate</div>
          <div class="value mono" id="cache-rate">—</div>
        </div>
        <div class="card">
          <div class="label">Estimated Savings</div>
          <div class="value mono" id="cache-savings">—</div>
        </div>
      </div>
      <table>
        <thead>
          <tr>
            <th>Pattern Hash</th>
            <th class="num">Hits</th>
            <th class="num">Tokens Saved</th>
          </tr>
        </thead>
        <tbody id="patterns-body">
          <tr><td colspan="3" class="skeleton">Loading…</td></tr>
        </tbody>
      </table>
    </section>

    <section>
      <h2>Circuit Breakers</h2>
      <div id="circuits">
        <span class="skeleton">Loading…</span>
      </div>
    </section>

    <section>
      <h2>Local Model Status</h2>
      <div id="local">
        <span class="skeleton">Loading…</span>
      </div>
    </section>

    <section>
      <h2>Local model endpoints</h2>
      <p class="muted">
        Multi-endpoint registry powered by <code>internal/localrouter</code>.
        Health checks run every 30 seconds; load + latency stats feed the
        routing strategies (round-robin, least-loaded, lowest-latency,
        priority). Configure at boot via <code>LENS_LOCAL_ENDPOINTS</code>
        or manage at runtime:
      </p>
      <ul class="mono" style="font-size:12px;color:var(--secondary);line-height:1.8">
        <li>GET    <span class="accent">/v1/local/endpoints</span> — list registered endpoints with health + EMA latency + error rate</li>
        <li>POST   <span class="accent">/v1/local/endpoints</span> — register a new endpoint dynamically</li>
        <li>DELETE <span class="accent">/v1/local/endpoints/&lt;id&gt;</span> — remove an endpoint</li>
        <li>POST   <span class="accent">/v1/local/endpoints/&lt;id&gt;/check</span> — trigger an immediate health probe</li>
      </ul>
    </section>

    <section>
      <h2>Workspaces</h2>
      <div id="workspaces">
        <span class="skeleton">Loading…</span>
      </div>
    </section>

    <section>
      <h2>Anomalies</h2>
      <div id="anomalies">
        <span class="skeleton">Loading…</span>
      </div>
    </section>

    <section>
      <h2>Git attribution</h2>
      <p class="muted">
        Workspace-scoped Git rollups (branch, PR, commit, author, repo)
        recorded automatically when callers send the X-Talyvor-* headers.
        Per-workspace dashboards consume these endpoints directly:
      </p>
      <ul class="mono" style="font-size:12px;color:var(--secondary);line-height:1.8">
        <li>GET <span class="accent">/v1/workspaces/&lt;wsID&gt;/attribution/branches</span><span class="muted"> ?since=…&amp;limit=20</span></li>
        <li>GET <span class="accent">/v1/workspaces/&lt;wsID&gt;/attribution/branches/&lt;branch&gt;</span></li>
        <li>GET <span class="accent">/v1/workspaces/&lt;wsID&gt;/attribution/prs/&lt;prNumber&gt;</span></li>
        <li>GET <span class="accent">/v1/workspaces/&lt;wsID&gt;/attribution/summary</span><span class="muted"> ?days=30</span></li>
      </ul>
    </section>
  </main>

  <footer>
    Talyvor Lens v{{VERSION}} · <a href="https://talyvor.com">talyvor.com</a> · <a href="https://github.com/talyvor/lens">GitHub</a>
  </footer>

  <script>
    const fmtUSD       = (n) => '$' + (Number(n) || 0).toLocaleString('en-US', {minimumFractionDigits: 2, maximumFractionDigits: 2});
    const fmtUSDsmall  = (n) => '$' + (Number(n) || 0).toLocaleString('en-US', {minimumFractionDigits: 4, maximumFractionDigits: 4});
    const fmtPct       = (n) => ((Number(n) || 0) * 100).toFixed(1) + '%';
    const fmtInt       = (n) => (Number(n) || 0).toLocaleString('en-US');

    // animateNumber counts a value up from the previously-rendered number.
    // Falls back to a plain set if the element has no parseable previous value.
    function animateNumber(el, finalText, prevValue, newValue) {
      const target = Number(newValue);
      const start  = Number(prevValue);
      if (!isFinite(target) || !isFinite(start) || target === start) {
        el.textContent = finalText;
        return;
      }
      const duration = 400;
      const t0 = performance.now();
      function step(now) {
        const t = Math.min(1, (now - t0) / duration);
        const v = start + (target - start) * t;
        el.dataset.value = String(v);
        el.textContent = finalText.replace(/[0-9.,]+/, () => v.toLocaleString('en-US', {minimumFractionDigits: 2, maximumFractionDigits: 2}));
        if (t < 1) requestAnimationFrame(step);
        else el.textContent = finalText;
      }
      requestAnimationFrame(step);
    }

    async function fetchJSON(url) {
      const r = await fetch(url, { headers: { 'Accept': 'application/json' }, cache: 'no-cache' });
      if (!r.ok) throw new Error('status ' + r.status);
      return r.json();
    }

    function applySpendSummary(d) {
      document.getElementById('spend-total').textContent       = fmtUSD(d.total_cost_usd);
      const hr = Number(d.cache_hit_rate) || 0;
      const hrEl = document.getElementById('spend-hitrate');
      hrEl.textContent = fmtPct(hr);
      hrEl.className   = 'value mono ' + (hr > 0.5 ? 'good' : hr > 0.25 ? 'warn' : 'bad');
      const savedTokens = ((Number(d.total_input_tokens) || 0) + (Number(d.total_output_tokens) || 0)) * hr;
      document.getElementById('spend-hitrate-sub').textContent = fmtInt(Math.round(savedTokens)) + ' tokens saved';
      document.getElementById('spend-requests').textContent    = fmtInt(d.total_requests);
      const days = Math.max(Number(d.period_days) || 30, 1);
      document.getElementById('spend-requests-sub').textContent = fmtInt(Math.round((Number(d.total_requests) || 0) / days)) + ' avg/day';
      document.getElementById('spend-avg').textContent         = fmtUSDsmall(d.avg_cost_per_request);
      const avg = Number(d.avg_cost_per_request) || 0;
      const withoutLens = hr < 1 ? avg / Math.max(1 - hr, 0.0001) : avg;
      document.getElementById('spend-avg-sub').textContent     = 'vs ' + fmtUSDsmall(withoutLens) + ' without Lens';
    }

    function applySpendByModel(rows) {
      const tbody = document.getElementById('models-body');
      if (!Array.isArray(rows) || rows.length === 0) {
        tbody.innerHTML = '<tr><td colspan="6" class="skeleton">No data yet</td></tr>';
        return;
      }
      const total = rows.reduce((s, r) => s + (Number(r.cost_usd) || 0), 0) || 1;
      tbody.innerHTML = rows.map(function (r) {
        const pct = (Number(r.cost_usd) || 0) / total;
        return '<tr>' +
          '<td class="mono">' + (r.model || '—') + '</td>' +
          '<td class="num mono">' + fmtInt(r.requests) + '</td>' +
          '<td class="num mono">' + fmtInt(r.input_tokens) + '</td>' +
          '<td class="num mono">' + fmtInt(r.output_tokens) + '</td>' +
          '<td class="num mono">' + fmtUSD(r.cost_usd) + '</td>' +
          '<td class="num mono">' + (pct * 100).toFixed(1) + '%<span class="bar" style="width:' + Math.max(2, pct * 80).toFixed(1) + 'px"></span></td>' +
        '</tr>';
      }).join('');
    }

    function applyCacheStats(d) {
      document.getElementById('cache-rate').textContent    = fmtPct(d.total_hit_rate);
      document.getElementById('cache-savings').textContent = fmtUSD(d.estimated_savings_usd);
    }

    function applyTopPatterns(rows) {
      const tbody = document.getElementById('patterns-body');
      if (!Array.isArray(rows) || rows.length === 0) {
        tbody.innerHTML = '<tr><td colspan="3" class="skeleton">No pinned patterns yet</td></tr>';
        return;
      }
      tbody.innerHTML = rows.map(function (r) {
        return '<tr>' +
          '<td class="mono">' + String(r.prompt_hash || '').slice(0, 8) + '…</td>' +
          '<td class="num mono">' + fmtInt(r.hit_count) + '</td>' +
          '<td class="num mono">' + fmtInt(r.tokens_saved) + '</td>' +
        '</tr>';
      }).join('');
    }

    function applyCircuits(map) {
      const root = document.getElementById('circuits');
      const entries = Object.entries(map || {});
      if (entries.length === 0) {
        root.innerHTML = '<span class="pill good">All circuits closed ✓</span>';
        return;
      }
      root.innerHTML =
        '<table><thead><tr><th>Team:Feature</th><th>Status</th></tr></thead><tbody>' +
        entries.map(function (kv) {
          const k = kv[0], v = kv[1];
          const cls = v === 'open' ? 'bad' : 'good';
          return '<tr><td class="mono">' + k + '</td><td><span class="pill ' + cls + '">' + String(v).toUpperCase() + '</span></td></tr>';
        }).join('') +
        '</tbody></table>';
    }

    function applyLocal(d) {
      const root = document.getElementById('local');
      if (d && d.available) {
        const models = ((d.models || []).map(function (m) { return m && m.name; }).filter(Boolean)).join(', ') || 'no models loaded';
        root.innerHTML = '<span class="pill good">Ollama available ✓</span> <span style="margin-left:12px;color:var(--secondary)">Models: ' + models + '</span>';
      } else {
        root.innerHTML = '<span class="pill warn">Ollama offline</span>';
      }
    }

    // Logging policy → pill class + label. Three states are surfaced
    // verbatim; anything else (legacy DB rows, schema drift) renders as
    // a neutral grey pill so the dashboard never lies about the policy.
    function loggingPolicyBadge(policy) {
      switch ((policy || '').toLowerCase()) {
        case 'full':     return '<span class="pill good">full</span>';
        case 'metadata': return '<span class="pill warn">metadata</span>';
        case 'none':     return '<span class="pill bad">Privacy mode</span>';
        default:         return '<span class="pill">unknown</span>';
      }
    }

    // Anomaly type → visual treatment. spike is loudest (red ⚠);
    // trend gets amber up-arrow; unusual is neutral yellow ~. Anything
    // else (new type added server-side) renders as a generic warn pill.
    function anomalyClass(type) {
      switch ((type || '').toLowerCase()) {
        case 'spike':   return 'bad';
        case 'trend':   return 'warn';
        case 'unusual': return 'warn';
        default:        return 'warn';
      }
    }
    function anomalyGlyph(type) {
      switch ((type || '').toLowerCase()) {
        case 'spike':   return '⚠';
        case 'trend':   return '↑';
        case 'unusual': return '~';
        default:        return '!';
      }
    }

    function applyAnomalies(list) {
      const root = document.getElementById('anomalies');
      if (!Array.isArray(list) || list.length === 0) {
        root.innerHTML = '<span class="pill good">No anomalies detected ✓</span>';
        return;
      }
      root.innerHTML = list.map(function (a) {
        const dim = [a.team, a.feature, a.provider].filter(Boolean).join(' · ') || a.workspace_id || '—';
        return '<div style="margin-bottom:10px;">' +
          '<span class="pill ' + anomalyClass(a.type) + '">' + anomalyGlyph(a.type) + ' ' + (a.type || '').toUpperCase() + '</span> ' +
          '<span style="margin-left:8px;color:var(--secondary)">' + dim + '</span>' +
          '<div style="margin-top:4px;color:var(--text);font-family:var(--mono);font-size:0.92rem;">' + (a.message || '') + '</div>' +
          '</div>';
      }).join('');
    }

    function applyWorkspaces(list) {
      const root = document.getElementById('workspaces');
      if (!Array.isArray(list) || list.length === 0) {
        root.innerHTML = '<span style="color:var(--secondary)">No workspaces registered.</span>';
        return;
      }
      root.innerHTML =
        '<table><thead><tr><th>Workspace</th><th>Logging</th><th>Active</th><th>Cost (30d)</th></tr></thead><tbody>' +
        list.map(function (ws) {
          return '<tr>' +
            '<td class="mono">' + (ws.name || ws.id) + '</td>' +
            '<td>' + loggingPolicyBadge(ws.logging_policy) + '</td>' +
            '<td>' + (ws.active ? '<span class="pill good">yes</span>' : '<span class="pill bad">no</span>') + '</td>' +
            '<td class="mono">' + fmtUSD(ws.current_month_cost_usd) + '</td>' +
            '</tr>';
        }).join('') +
        '</tbody></table>';
    }

    async function refresh() {
      const alertEl = document.getElementById('api-alert');
      let anyFail = false;
      const tries = [
        ['/v1/api/spend/summary?workspace_id=default&days=30',     applySpendSummary],
        ['/v1/api/spend/by-model?workspace_id=default&days=30',    applySpendByModel],
        ['/v1/api/cache/stats?workspace_id=default',               applyCacheStats],
        ['/v1/api/cache/top-patterns?workspace_id=default&limit=10', applyTopPatterns],
        ['/v1/api/alerts/circuits',                                 applyCircuits],
        ['/v1/api/local/status',                                    applyLocal],
        ['/v1/api/workspaces',                                      applyWorkspaces],
        ['/v1/api/anomalies/scan',                                  applyAnomalies],
      ];
      await Promise.all(tries.map(async function (entry) {
        const url = entry[0], fn = entry[1];
        try {
          const data = await fetchJSON(url);
          fn(data);
        } catch (err) {
          anyFail = true;
        }
      }));
      alertEl.classList.toggle('visible', anyFail);
      document.getElementById('updated').textContent = 'updated ' + new Date().toLocaleTimeString();
    }

    refresh();
    setInterval(refresh, 30000);
  </script>
</body>
</html>`
