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
    <nav style="display:flex;gap:14px;font-size:12px;margin-left:24px">
      <a href="/dashboard" style="color:var(--accent);text-decoration:none">Overview</a>
      <a href="/dashboard/tokens" style="color:var(--secondary);text-decoration:none">Tokens &amp; Mining</a>
      <a href="/dashboard/nodes" style="color:var(--secondary);text-decoration:none">Nodes</a>
      <a href="/dashboard/economy" style="color:var(--secondary);text-decoration:none">Economy</a>
    </nav>
    <span class="updated mono" id="updated" style="margin-left:auto">—</span>
  </header>

  <main>
    <div class="alert" id="api-alert">
      ⚠ Unable to reach Lens API. Configure your API key (header <code class="mono">Authorization: Bearer &lt;key&gt;</code>) to view live data.
    </div>

    <section id="lens-widget" style="display:none">
      <h2>🪙 LENS Economy</h2>
      <div class="cards">
        <div class="card">
          <div class="label">Total Supply</div>
          <div class="value mono accent" id="lens-supply">—</div>
          <div class="sub">all-time minted</div>
        </div>
        <div class="card">
          <div class="label">Circulating</div>
          <div class="value mono" id="lens-circulating">—</div>
          <div class="sub">supply − burned</div>
        </div>
        <div class="card">
          <div class="label">Avg Listing Price</div>
          <div class="value mono" id="lens-price">—</div>
          <div class="sub">marketplace</div>
        </div>
        <div class="card">
          <div class="label">Active Listings</div>
          <div class="value mono" id="lens-listings">—</div>
          <div class="sub"><a href="/dashboard/economy" style="color:var(--accent)">view all →</a></div>
        </div>
      </div>
      <script>
      fetch('/v1/economy/stats').then(r => r.ok ? r.json() : null).then(s => {
        if (!s) return;
        document.getElementById('lens-widget').style.display = '';
        document.getElementById('lens-supply').textContent = (s.total_supply || 0).toFixed(2) + ' LENS';
        document.getElementById('lens-circulating').textContent = (s.circulating_supply || 0).toFixed(2) + ' LENS';
        document.getElementById('lens-price').textContent = '$' + (s.avg_price_usd || 0).toFixed(4) + '/LENS';
        document.getElementById('lens-listings').textContent = s.market_listings;
      });
      </script>
    </section>

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

    <section id="cluster-panel" style="display:none">
      <h2>Cluster <span class="muted" style="font-size:12px">· high availability</span></h2>
      <div id="cluster">
        <span class="skeleton">Loading…</span>
      </div>
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

    <section id="budgets-panel" style="display:none">
      <h2>Budgets</h2>
      <p class="muted">
        Per-workspace / team / sprint spend governance. Enforcement is
        <code>off</code> (track only), <code>alert</code> (default), or
        <code>hard_block</code> (reject over the limit). Manage via
        <span class="accent">/v1/workspaces/&lt;id&gt;/budgets</span>.
      </p>
      <div id="budgets">
        <span class="skeleton">Loading…</span>
      </div>
    </section>

    <section id="forecast-panel" style="display:none">
      <h2>Cost forecast <span class="pill" style="background:#3b3b52;color:#cfcfe6">projection</span></h2>
      <p class="muted">
        Projected period spend at the current pace — <em>estimates, not
        actual spend</em>. Linear run-rate (spend-so-far ÷ elapsed fraction)
        with a trailing-window daily rate + trend. Low-data periods are
        flagged. Projected figures shown in <em>italic</em> with “≈”.
      </p>
      <div id="forecast">
        <span class="skeleton">Loading…</span>
      </div>
    </section>

    <section id="challenges-panel" style="display:none">
      <h2>Challenges <span class="pill" style="background:#3b3b52;color:#cfcfe6">PoVI proof-of-retention</span></h2>
      <p class="muted">
        Random Merkle-path challenges. A node that can't prove sampled positions
        (bad path or no answer) is <span class="pill bad">slashed</span>.
        Security is <em>economic + probabilistic</em>: expected cost of cheating
        = P(challenge) × slash &gt; gain — not an absolute proof of honest
        computation.
      </p>
      <div id="challenges">
        <span class="skeleton">Loading…</span>
      </div>
    </section>

    <section id="staking-panel" style="display:none">
      <h2>Staking <span class="pill" style="background:#3b3b52;color:#cfcfe6">PoVI collateral</span></h2>
      <p class="muted">
        Node-registration collateral (LENS locked). A node must stake ≥ the
        minimum to be <em>minting-eligible</em>; collateral is slashable while
        active <em>or</em> unbonding (the anti-yank property). Slashed
        collateral is <span class="pill bad">burned</span>. Minting itself stays
        OFF until challenge-and-slash (Part 3).
      </p>
      <div id="staking">
        <span class="skeleton">Loading…</span>
      </div>
    </section>

    <section id="eval-panel" style="display:none">
      <h2>Evaluation <span class="pill" style="background:#3b3b52;color:#cfcfe6">golden datasets</span></h2>
      <p class="muted">
        Recent eval runs — pass rate against the 80% gate. Runs below the gate
        are flagged <span class="pill bad">⚠</span>. Per-case regressions
        (vs the prior run on the same dataset) and A/B significance verdicts —
        with an honest <em>“inconclusive”</em> when the sample is too small to
        call a winner — are served on demand by the eval API.
      </p>
      <div id="eval-runs">
        <span class="skeleton">Loading…</span>
      </div>
    </section>

    <section>
      <h2>Anomalies</h2>
      <div id="anomalies">
        <span class="skeleton">Loading…</span>
      </div>
    </section>

    <section id="costoutliers-panel" style="display:none">
      <h2>Cost outliers <span class="pill" style="background:#3b3b52;color:#cfcfe6">statistical flag</span></h2>
      <p class="muted">
        Units of work whose cost is far above the median of comparable units
        (same workspace, trailing window) by robust statistics (median +
        MAD). These are <em>statistical flags, not judgments</em> — "4× the
        median" is a fact, not a verdict that anything is wrong. Hidden when
        the baseline is too small to judge.
      </p>
      <div id="costoutliers">
        <span class="skeleton">Loading…</span>
      </div>
    </section>

    <section id="roi-panel" style="display:none">
      <h2>Executive summary <span class="pill" style="background:#3b3b52;color:#cfcfe6">CFO-ready</span></h2>
      <p class="muted">
        Headline AI-cost summary for the budget-holder. Engineer-level
        breakdown is hidden unless explicitly enabled (it's a cost
        attribution, not a performance judgment).
        <a id="roi-fullreport" href="#" target="_blank">View full report (HTML)</a>.
      </p>
      <div id="roi-summary">
        <span class="skeleton">Loading…</span>
      </div>
    </section>

    <section id="routing-panel" style="display:none">
      <h2>Routing intelligence <span class="pill" style="background:#3b3b52;color:#cfcfe6">data-driven</span></h2>
      <p class="muted">
        Best quality-per-dollar model per feature category, learned from the
        opted-in pattern network. <em>Data-driven suggestions</em> — applied
        only to requests that cede the model choice (model "auto" /
        X-Talyvor-Auto-Route), within the workspace's allowed models, and
        only above the sample floor. Hidden when disabled.
      </p>
      <div id="routing-intel">
        <span class="skeleton">Loading…</span>
      </div>
    </section>

    <section>
      <h2>Model capabilities <span class="pill" style="background:#3b3b52;color:#cfcfe6">multimodal</span></h2>
      <p class="muted">
        Which models can serve which modalities. A multimodal request is
        routed to a capable model (or fails fast with a clear error) — never
        silently answered from text. Unknown models are treated as text-only.
      </p>
      <div id="modality-caps">
        <span class="skeleton">Loading…</span>
      </div>
    </section>

    <section>
      <h2>Model catalog <span class="pill" style="background:#3b3b52;color:#cfcfe6">single source</span></h2>
      <p class="muted">
        The authoritative model registry — provider, pricing ($/1M tokens),
        capabilities, and context limits. Cost attribution, capability
        routing, and introspection all read from here.
      </p>
      <div id="model-catalog">
        <span class="skeleton">Loading…</span>
      </div>
    </section>

    <section id="guardrails-panel" style="display:none">
      <h2>Guardrails <span class="pill" style="background:#3b3b52;color:#cfcfe6">safety</span></h2>
      <p class="muted">
        Per-workspace input + output safety rules (PII, prompt-injection,
        content policy, output validation). Block fails fast with a clear
        error; redact masks and continues; flag observes. Output guardrails
        don't apply to streamed responses unless the workspace opts into
        buffering. Hidden when the output stage is disabled.
      </p>
      <div id="guardrails-cfg">
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

    function fmtDur(ms) {
      if (ms < 0) ms = 0;
      var s = Math.floor(ms / 1000);
      if (s < 60) return s + 's';
      var m = Math.floor(s / 60);
      if (m < 60) return m + 'm';
      var h = Math.floor(m / 60);
      if (h < 24) return h + 'h';
      return Math.floor(h / 24) + 'd';
    }

    // applyCluster renders the HA cluster panel from /ha/status. The panel is
    // hidden entirely when HA is disabled (status.enabled === false).
    function applyCluster(d) {
      var panel = document.getElementById('cluster-panel');
      if (!d || !d.enabled) { panel.style.display = 'none'; return; }
      panel.style.display = '';
      var root = document.getElementById('cluster');
      var insts = d.instances || [];
      if (insts.length === 0) {
        root.innerHTML = '<span class="pill warn">No instances registered</span>';
        return;
      }
      var now = Date.now();
      root.innerHTML =
        '<table><thead><tr><th>Instance</th><th>Host</th><th>Status</th><th>Version</th><th>Uptime</th><th>Last seen</th></tr></thead><tbody>' +
        insts.map(function (it) {
          var st = String(it.status || '');
          var cls = st === 'active' ? 'good' : (st === 'draining' ? 'warn' : 'bad');
          var started = it.started_at ? new Date(it.started_at).getTime() : now;
          var seen = it.last_seen ? new Date(it.last_seen).getTime() : now;
          return '<tr><td class="mono">' + String(it.id || '').slice(0, 8) +
            '</td><td class="mono">' + (it.host || '&mdash;') +
            '</td><td><span class="pill ' + cls + '">' + st.toUpperCase() + '</span></td>' +
            '<td class="mono">' + (it.version || '&mdash;') + '</td>' +
            '<td class="mono">' + fmtDur(now - started) + '</td>' +
            '<td class="mono">' + fmtDur(now - seen) + ' ago</td></tr>';
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

    function applyBudgets(list) {
      const panel = document.getElementById('budgets-panel');
      // The panel hides itself entirely when no budgets are configured.
      if (!Array.isArray(list) || list.length === 0) {
        panel.style.display = 'none';
        return;
      }
      panel.style.display = '';
      document.getElementById('budgets').innerHTML =
        '<table><thead><tr><th>Scope</th><th>ID</th><th>Period</th><th>Spent / Limit</th><th>Utilization</th><th>Enforcement</th></tr></thead><tbody>' +
        list.map(function (b) {
          const limit = b.limit_usd || 0;
          const spent = b.spent_usd || 0;
          const ratio = limit > 0 ? spent / limit : 0;
          const cls = ratio >= 1 ? 'bad' : 'good';
          const pct = limit > 0 ? (ratio * 100).toFixed(0) + '%' : '—';
          return '<tr>' +
            '<td>' + b.scope + '</td>' +
            '<td class="mono">' + (b.scope_id || '—') + '</td>' +
            '<td>' + b.period + '</td>' +
            '<td class="mono">' + fmtUSD(spent) + ' / ' + fmtUSD(limit) + '</td>' +
            '<td><span class="pill ' + cls + '">' + pct + '</span></td>' +
            '<td>' + b.enforcement + '</td>' +
            '</tr>';
        }).join('') +
        '</tbody></table>';
    }

    function applyChallenges(list) {
      const panel = document.getElementById('challenges-panel');
      if (!Array.isArray(list) || list.length === 0) {
        panel.style.display = 'none';
        return;
      }
      panel.style.display = '';
      const badge = function (r) {
        if (r === 'pass') return '<span class="pill good">pass</span>';
        if (r === 'fail') return '<span class="pill bad">fail</span>';
        if (r === 'timeout') return '<span class="pill bad">timeout</span>';
        return '<span class="pill">' + r + '</span>';
      };
      document.getElementById('challenges').innerHTML =
        '<table><thead><tr><th>Challenge</th><th>Node</th><th>Request</th><th>Positions</th><th>Result</th><th>Slashed</th><th>When</th></tr></thead><tbody>' +
        list.map(function (c) {
          return '<tr>' +
            '<td class="mono">' + String(c.id || '').slice(0, 12) + '</td>' +
            '<td class="mono">' + String(c.node_id || '').slice(0, 12) + '</td>' +
            '<td class="mono">' + String(c.request_id || '').slice(0, 12) + '</td>' +
            '<td class="mono">' + ((c.positions || []).join(',')) + '</td>' +
            '<td>' + badge(c.result) + '</td>' +
            '<td class="mono">' + (c.slashed_amount ? (c.slashed_amount).toFixed(3) : '—') + '</td>' +
            '<td>' + (c.created_at ? new Date(c.created_at).toLocaleString() : '—') + '</td>' +
          '</tr>';
        }).join('') + '</tbody></table>';
    }

    function applyStakes(list) {
      const panel = document.getElementById('staking-panel');
      if (!Array.isArray(list) || list.length === 0) {
        panel.style.display = 'none';
        return;
      }
      panel.style.display = '';
      const badge = function (s) {
        if (s === 'active') return '<span class="pill good">active</span>';
        if (s === 'unbonding') return '<span class="pill" style="background:#5a4b1f;color:#f0d98c">unbonding</span>';
        if (s === 'slashed') return '<span class="pill bad">slashed</span>';
        return '<span class="pill">' + s + '</span>';
      };
      document.getElementById('staking').innerHTML =
        '<table><thead><tr><th>Node</th><th>Workspace</th><th>Locked LENS</th><th>Status</th><th>Slashed</th><th>Unbond at</th></tr></thead><tbody>' +
        list.map(function (s) {
          return '<tr>' +
            '<td class="mono">' + String(s.node_id || '').slice(0, 12) + '</td>' +
            '<td class="mono">' + (s.workspace_id || '—') + '</td>' +
            '<td class="mono">' + (s.amount || 0).toFixed(3) + '</td>' +
            '<td>' + badge(s.status) + '</td>' +
            '<td class="mono">' + (s.slashed_amount ? (s.slashed_amount).toFixed(3) : '—') + '</td>' +
            '<td>' + (s.unbond_at ? new Date(s.unbond_at).toLocaleString() : '—') + '</td>' +
          '</tr>';
        }).join('') + '</tbody></table>';
    }

    function applyEvalRuns(list) {
      const panel = document.getElementById('eval-panel');
      // Hidden entirely when there are no eval runs.
      if (!Array.isArray(list) || list.length === 0) {
        panel.style.display = 'none';
        return;
      }
      panel.style.display = '';
      document.getElementById('eval-runs').innerHTML =
        '<table><thead><tr><th>Run</th><th>When</th><th>Tests</th><th>Passed</th><th>Failed</th><th>Pass rate</th><th>Cost</th></tr></thead><tbody>' +
        list.map(function (r) {
          const rate = r.pass_rate || 0;
          const ratePill = rate >= 0.8
            ? '<span class="pill good">' + (rate * 100).toFixed(0) + '%</span>'
            : '<span class="pill bad">' + (rate * 100).toFixed(0) + '% ⚠</span>';
          return '<tr>' +
            '<td class="mono">' + String(r.run_id || '').slice(0, 8) + '</td>' +
            '<td>' + (r.created_at ? new Date(r.created_at).toLocaleString() : '—') + '</td>' +
            '<td class="mono">' + (r.total_tests || 0) + '</td>' +
            '<td class="mono">' + (r.passed || 0) + '</td>' +
            '<td class="mono">' + (r.failed || 0) + '</td>' +
            '<td>' + ratePill + '</td>' +
            '<td class="mono">' + fmtUSD(r.total_cost_usd || 0) + '</td>' +
          '</tr>';
        }).join('') + '</tbody></table>';
    }

    function applyForecast(list) {
      const panel = document.getElementById('forecast-panel');
      // Hidden when there are no budgets to project (or no data).
      if (!Array.isArray(list) || list.length === 0) {
        panel.style.display = 'none';
        return;
      }
      panel.style.display = '';
      document.getElementById('forecast').innerHTML =
        '<table><thead><tr><th>Scope</th><th>ID</th><th>Period</th><th>Spent so far</th><th>Projected total</th><th>vs Limit</th><th>Trend</th><th>Est. exhaustion</th><th>Confidence</th></tr></thead><tbody>' +
        list.map(function (f) {
          const vb = f.vs_budget || {};
          // Projections are italic + "≈" so they never read as actual spend.
          const projCell = f.insufficient_data
            ? '<em class="muted">insufficient data</em>'
            : '<em>≈ ' + fmtUSD(f.projected_total_usd) + '</em>';
          let limitCell;
          if (vb.limit_usd) {
            if (f.insufficient_data) {
              limitCell = '<span class="muted">' + fmtUSD(vb.limit_usd) + ' limit</span>';
            } else if (vb.will_exceed) {
              limitCell = '<span class="pill bad">over by ' + fmtUSD(vb.projected_overage_usd) + '</span>';
            } else {
              limitCell = '<span class="pill good">' + ((vb.projected_utilization || 0) * 100).toFixed(0) + '% of ' + fmtUSD(vb.limit_usd) + '</span>';
            }
          } else {
            limitCell = '<span class="muted">no budget</span>';
          }
          const exhaustCell = (vb.est_exhaustion_date && !f.insufficient_data)
            ? '<em>' + new Date(vb.est_exhaustion_date).toLocaleDateString() + '</em>'
            : '—';
          return '<tr>' +
            '<td>' + f.scope + '</td>' +
            '<td class="mono">' + (f.scope_id || '—') + '</td>' +
            '<td>' + f.period + '</td>' +
            '<td class="mono">' + fmtUSD(f.spent_so_far_usd) + '</td>' +
            '<td class="mono">' + projCell + '</td>' +
            '<td>' + limitCell + '</td>' +
            '<td>' + (f.trend_label || 'unknown') + '</td>' +
            '<td class="mono">' + exhaustCell + '</td>' +
            '<td class="muted" style="font-size:12px">' + (f.confidence_note || '') + '</td>' +
            '</tr>';
        }).join('') +
        '</tbody></table>';
    }

    function applyCostOutliers(res) {
      const panel = document.getElementById('costoutliers-panel');
      const anomalies = res && res.anomalies;
      // Hidden when the baseline is insufficient or nothing is flagged.
      if (!res || res.insufficient_baseline || !Array.isArray(anomalies) || anomalies.length === 0) {
        panel.style.display = 'none';
        return;
      }
      panel.style.display = '';
      document.getElementById('costoutliers').innerHTML =
        '<p class="muted" style="font-size:12px">Baseline median ' + fmtUSD(res.baseline_median) +
        ' · threshold ' + fmtUSD(res.threshold_usd) + ' · ' + res.sample_size + ' comparable ' + res.scope + 's · method ' + res.method + '</p>' +
        '<table><thead><tr><th>Unit</th><th>Cost</th><th>× median</th><th>Severity</th><th>Flag</th></tr></thead><tbody>' +
        anomalies.map(function (a) {
          const cls = a.severity === 'high' ? 'bad' : (a.severity === 'warn' ? 'warn' : 'good');
          return '<tr>' +
            '<td class="mono">' + a.unit_id + '</td>' +
            '<td class="mono">' + fmtUSD(a.cost_usd) + '</td>' +
            '<td class="mono">' + (a.factor || 0).toFixed(1) + '×</td>' +
            '<td><span class="pill ' + cls + '">' + a.severity + '</span></td>' +
            '<td class="muted" style="font-size:12px">' + (a.explanation || '') + '</td>' +
            '</tr>';
        }).join('') +
        '</tbody></table>';
    }

    function applyROISummary(s) {
      const panel = document.getElementById('roi-panel');
      if (!s || s.insufficient_data) {
        panel.style.display = 'none';
        return;
      }
      panel.style.display = '';
      // Point the "full report" link at the HTML format for this period.
      const link = document.getElementById('roi-fullreport');
      link.setAttribute('href', '/v1/workspaces/' + (s.workspace_id || 'default') + '/roi/report?format=html&period=' + (s.period || 'monthly'));
      const teams = (s.top_teams || []).map(function (t) {
        return '<span class="pill good" style="margin-right:6px">' + t.team + ' ' + fmtUSD(t.cost_usd) + '</span>';
      }).join('');
      const proj = s.forecast_will_exceed
        ? '<span class="pill bad">≈ ' + fmtUSD(s.projected_total_usd) + ' (over budget)</span>'
        : '<span class="pill good">≈ ' + fmtUSD(s.projected_total_usd) + '</span>';
      document.getElementById('roi-summary').innerHTML =
        '<div class="grid">' +
        '<div class="stat"><div class="label">Total spend (' + (s.period || '') + ')</div><div class="value">' + fmtUSD(s.total_spend_usd) + '</div></div>' +
        '<div class="stat"><div class="label">vs previous</div><div class="value">' + (s.pct_change_vs_prev >= 0 ? '+' : '') + (s.pct_change_vs_prev || 0).toFixed(1) + '%</div></div>' +
        '<div class="stat"><div class="label">Projected period total</div><div class="value">' + proj + '</div></div>' +
        '<div class="stat"><div class="label">Budgets over</div><div class="value">' + (s.budgets_over_count || 0) + '</div></div>' +
        '<div class="stat"><div class="label">Cost outliers</div><div class="value">' + (s.anomaly_count || 0) + '</div></div>' +
        '</div>' +
        '<p class="muted" style="margin-top:8px">Top teams: ' + (teams || '—') + '</p>';
    }

    function applyRoutingIntel(d) {
      const panel = document.getElementById('routing-panel');
      const st = d && d.status;
      // Hidden when intelligence is disabled.
      if (!st || !st.enabled) {
        panel.style.display = 'none';
        return;
      }
      panel.style.display = '';
      const cohorts = (d.cohorts || []);
      const meta = '<p class="muted" style="font-size:12px">' + cohorts.length + ' cohorts · floor ≥' + st.min_samples +
        ' samples / ≥' + st.min_workspaces + ' workspaces · refreshed ' +
        (st.last_refresh ? new Date(st.last_refresh).toLocaleTimeString() : '—') + '</p>';
      let body;
      if (cohorts.length === 0) {
        body = '<span class="muted">No cohorts yet — not enough opted-in pattern data.</span>';
      } else {
        body = '<table><thead><tr><th>Feature</th><th>Input</th><th>Recommended</th><th>Quality</th><th>$/1k</th><th>Q/$</th><th>Samples</th><th>Status</th></tr></thead><tbody>' +
          cohorts.map(function (c) {
            const cls = c.qualifies ? 'good' : 'warn';
            const label = c.qualifies ? 'active' : 'below floor';
            return '<tr>' +
              '<td>' + c.feature + '</td>' +
              '<td>' + c.input_range + '</td>' +
              '<td class="mono">' + c.model + '</td>' +
              '<td class="mono">' + (c.avg_quality || 0).toFixed(2) + '</td>' +
              '<td class="mono">' + fmtUSD(c.cost_per_1k) + '</td>' +
              '<td class="mono">' + (c.quality_per_dollar || 0).toFixed(1) + '</td>' +
              '<td class="mono">' + c.sample_count + ' / ' + c.distinct_workspaces + 'ws</td>' +
              '<td><span class="pill ' + cls + '">' + label + '</span></td>' +
              '</tr>';
          }).join('') +
          '</tbody></table>';
      }
      document.getElementById('routing-intel').innerHTML = meta + body;
    }

    function applyModalityCaps(map) {
      const models = Object.keys(map || {}).sort();
      const yn = function (b) { return b ? '<span class="pill good">✓</span>' : '<span class="muted">—</span>'; };
      const root = document.getElementById('modality-caps');
      if (models.length === 0) {
        root.innerHTML = '<span class="muted">No capability data.</span>';
        return;
      }
      root.innerHTML =
        '<table><thead><tr><th>Model</th><th>Vision</th><th>Audio</th><th>Document</th></tr></thead><tbody>' +
        models.map(function (m) {
          const c = map[m] || {};
          return '<tr><td class="mono">' + m + '</td><td>' + yn(c.vision) + '</td><td>' + yn(c.audio) + '</td><td>' + yn(c.document) + '</td></tr>';
        }).join('') +
        '</tbody></table>';
    }

    function applyCatalog(models) {
      const root = document.getElementById('model-catalog');
      if (!Array.isArray(models) || models.length === 0) {
        root.innerHTML = '<span class="muted">No catalog data.</span>';
        return;
      }
      const yn = function (b) { return b ? '✓' : '—'; };
      root.innerHTML =
        '<table><thead><tr><th>Model</th><th>Provider</th><th>$/1M in</th><th>$/1M out</th><th>Vision</th><th>Audio</th><th>Doc</th><th>Context</th></tr></thead><tbody>' +
        models.map(function (m) {
          const c = m.capabilities || {};
          return '<tr>' +
            '<td class="mono">' + m.id + (m.deprecated ? ' <span class="pill warn">deprecated</span>' : '') + '</td>' +
            '<td>' + m.provider + '</td>' +
            '<td class="mono">' + fmtUSD(m.input_per_1m) + '</td>' +
            '<td class="mono">' + fmtUSD(m.output_per_1m) + '</td>' +
            '<td>' + yn(c.vision) + '</td><td>' + yn(c.audio) + '</td><td>' + yn(c.document) + '</td>' +
            '<td class="mono">' + (m.context_tokens ? (m.context_tokens / 1000) + 'K' : '—') + '</td>' +
            '</tr>';
        }).join('') +
        '</tbody></table>';
    }

    function applyGuardrails(d) {
      const panel = document.getElementById('guardrails-panel');
      // Hidden when the output stage is disabled.
      if (!d || !d.enabled) {
        panel.style.display = 'none';
        return;
      }
      panel.style.display = '';
      const p = d.policy || {};
      const yn = function (b) { return b ? '<span class="pill good">on</span>' : '<span class="muted">off</span>'; };
      const act = function (a) { return a ? '<span class="pill ' + (a === 'block' ? 'bad' : 'warn') + '">' + a + '</span>' : '<span class="muted">—</span>'; };
      const rows = [
        ['Input PII', yn(p.enable_pii), act(p.pii_action)],
        ['Prompt injection', yn(p.enable_injection), act(p.injection_action)],
        ['Blocked topics', yn(p.enable_topics), (p.blocked_topics || []).length + ' configured'],
        ['Word filter', yn(p.enable_word_filter), (p.blocked_words || []).length + ' configured'],
        ['Custom rules', '—', (p.custom_rules || []).length + ' configured'],
        ['Output PII', p.output_pii_action ? yn(true) : yn(false), act(p.output_pii_action)],
        ['Output validate JSON', yn(p.output_validate_json), p.output_validation_block ? '<span class="pill bad">block</span>' : '<span class="pill warn">flag</span>'],
        ['Output max length', p.output_max_length ? yn(true) : yn(false), (p.output_max_length || 0) + ' chars'],
        ['Stream buffering', yn(p.buffer_stream_for_output), p.buffer_stream_for_output ? 'output guardrails on streams' : 'streams not inspected'],
      ];
      document.getElementById('guardrails-cfg').innerHTML =
        '<table><thead><tr><th>Rule</th><th>Status</th><th>Action / detail</th></tr></thead><tbody>' +
        rows.map(function (r) { return '<tr><td>' + r[0] + '</td><td>' + r[1] + '</td><td>' + r[2] + '</td></tr>'; }).join('') +
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
        ['/ha/status',                                              applyCluster],
        ['/v1/api/local/status',                                    applyLocal],
        ['/v1/api/workspaces',                                      applyWorkspaces],
        ['/v1/api/anomalies/scan',                                  applyAnomalies],
        ['/v1/api/budgets?workspace_id=default',                    applyBudgets],
        ['/v1/api/forecast/summary?workspace_id=default',           applyForecast],
        ['/v1/api/costanomalies?workspace_id=default',              applyCostOutliers],
        ['/v1/api/roi/summary?workspace_id=default',                applyROISummary],
        ['/v1/api/routing/intelligence',                            applyRoutingIntel],
        ['/v1/api/modality/capabilities',                           applyModalityCaps],
        ['/v1/api/catalog',                                         applyCatalog],
        ['/v1/api/eval/runs?workspace_id=default',                  applyEvalRuns],
        ['/v1/api/povi/stakes',                                     applyStakes],
        ['/v1/api/povi/challenges',                                 applyChallenges],
        ['/v1/api/guardrails?workspace_id=default',                 applyGuardrails],
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
