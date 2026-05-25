package dashboard

// token_dashboard.go — three additional static-HTML pages that
// extend the main Lens dashboard with the LENS-token surface:
//   /dashboard/tokens    — per-workspace balance + mining + stake + marketplace
//   /dashboard/nodes     — registered inference / cache / embedding nodes
//   /dashboard/economy   — global supply / circulation / top miners
//
// All three follow the same convention as ui.go: a single HTML
// template rendered once at construction time (with {{VERSION}}
// substituted), zero external dependencies, dark theme via the
// shared CSS palette, fetch() calls into the existing JSON API.
//
// The pages take the workspace ID from the `?ws=<id>` query
// string when present — keeps deep-links shareable. Falls back
// to a "select a workspace" hint when empty.

import (
	"net/http"
	"strings"
)

// ─── helpers shared by the three pages ──────────

// renderPage performs the {{VERSION}} substitution + writes the
// HTTP response with the right content type. Cheap memcpy on
// every request — no template engine.
func renderPage(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// renderTemplate is the lazy variant the per-page handlers use:
// at construction time we cache the rendered byte slice on the
// Handler, but we keep the original string-only renderer here so
// tests can poke specific substitutions.
func renderTemplate(html, version string) []byte {
	return []byte(strings.ReplaceAll(html, "{{VERSION}}", version))
}

// ─── extended Handler with token-page bodies ────

// TokensPage / NodesPage / EconomyPage are the per-page renders
// the Handler caches at construction. They're declared on the
// existing Handler struct so cmd/lens/main.go can wire all four
// routes from one shared instance.
type tokenPages struct {
	tokensHTML  []byte
	nodesHTML   []byte
	economyHTML []byte
}

// initTokenPages computes the three byte slices once. The main
// `New` constructor in handler.go calls this after the legacy
// page is rendered.
func (h *Handler) initTokenPages() {
	if h.tokenPages != nil {
		return
	}
	h.tokenPages = &tokenPages{
		tokensHTML:  renderTemplate(tokensDashboardHTML, h.version),
		nodesHTML:   renderTemplate(nodesDashboardHTML, h.version),
		economyHTML: renderTemplate(economyDashboardHTML, h.version),
	}
}

// ServeTokens is the GET /dashboard/tokens route.
func (h *Handler) ServeTokens(w http.ResponseWriter, _ *http.Request) {
	h.initTokenPages()
	renderPage(w, h.tokenPages.tokensHTML)
}

// ServeNodes is GET /dashboard/nodes.
func (h *Handler) ServeNodes(w http.ResponseWriter, _ *http.Request) {
	h.initTokenPages()
	renderPage(w, h.tokenPages.nodesHTML)
}

// ServeEconomy is GET /dashboard/economy.
func (h *Handler) ServeEconomy(w http.ResponseWriter, _ *http.Request) {
	h.initTokenPages()
	renderPage(w, h.tokenPages.economyHTML)
}

// ─── shared HTML constants ──────────────────────

// commonHead is the <head> + <style> chunk every page reuses.
// Kept as a string constant so the page-specific templates can
// drop it into their own bodies without going through a real
// template engine.
const commonHead = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Talyvor Lens · Tokens</title>
  <meta name="theme-color" content="#0c0e12">
  <style>
    :root {
      --bg: #0c0e12;
      --panel: #14171e;
      --border: #1c1f26;
      --text: #d4d8e2;
      --secondary: #8892a4;
      --accent: #f0a030;
      --good: #5ac17d;
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
      gap: 24px;
      flex-wrap: wrap;
    }
    header h1 {
      margin: 0;
      font-size: 18px;
      letter-spacing: 0.08em;
      text-transform: uppercase;
    }
    nav { display: flex; gap: 16px; font-size: 13px; }
    nav a {
      color: var(--secondary);
      text-decoration: none;
      padding: 4px 8px;
      border-radius: 4px;
    }
    nav a:hover { color: var(--text); background: var(--panel); }
    nav a.active { color: var(--accent); }
    main { padding: 24px 32px; max-width: 1200px; margin: 0 auto; }
    section {
      background: var(--panel);
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 24px;
      margin-bottom: 24px;
    }
    section h2 {
      margin: 0 0 16px 0;
      font-size: 15px;
      letter-spacing: 0.04em;
      text-transform: uppercase;
      color: var(--secondary);
    }
    .grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
      gap: 16px;
    }
    .stat {
      padding: 12px 0;
      border-bottom: 1px dashed var(--border);
    }
    .stat:last-child { border-bottom: none; }
    .stat .label { font-size: 12px; color: var(--secondary); text-transform: uppercase; letter-spacing: 0.08em; }
    .stat .value { font-size: 22px; font-family: 'IBM Plex Mono', monospace; margin-top: 4px; }
    .stat .value.lens { color: var(--accent); }
    .stat .usd { font-size: 12px; color: var(--secondary); margin-left: 6px; }
    table { width: 100%; border-collapse: collapse; font-size: 13px; }
    th { text-align: left; padding: 8px 12px; font-weight: 500; color: var(--secondary); border-bottom: 1px solid var(--border); }
    td { padding: 8px 12px; border-bottom: 1px dashed var(--border); font-family: 'IBM Plex Mono', monospace; }
    tr:last-child td { border-bottom: none; }
    button {
      background: var(--accent);
      color: #1c1410;
      border: none;
      border-radius: 6px;
      padding: 8px 16px;
      font-size: 13px;
      font-weight: 600;
      cursor: pointer;
      font-family: inherit;
    }
    button:hover { filter: brightness(1.1); }
    button.secondary { background: var(--panel); color: var(--text); border: 1px solid var(--border); }
    input, select {
      background: var(--bg);
      border: 1px solid var(--border);
      color: var(--text);
      border-radius: 6px;
      padding: 8px 12px;
      font-size: 13px;
      font-family: inherit;
    }
    .form-row { display: flex; gap: 12px; flex-wrap: wrap; align-items: center; margin-bottom: 12px; }
    .form-row label { font-size: 12px; color: var(--secondary); min-width: 100px; }
    .toast {
      position: fixed;
      bottom: 24px;
      right: 24px;
      padding: 12px 18px;
      border-radius: 6px;
      background: var(--panel);
      border: 1px solid var(--border);
      font-size: 13px;
      box-shadow: 0 4px 16px rgba(0,0,0,0.4);
      display: none;
    }
    .toast.good { border-color: var(--good); }
    .toast.bad  { border-color: var(--bad); }
    footer {
      padding: 16px 32px;
      border-top: 1px solid var(--border);
      color: var(--secondary);
      font-size: 12px;
      text-align: center;
    }
  </style>
</head>`

// commonHeader is the <header>+<nav> chunk every page reuses.
// The active link is replaced via a placeholder string when the
// individual pages embed it.
const commonHeader = `<header>
  <h1>Talyvor Lens</h1>
  <nav>
    <a href="/dashboard">Overview</a>
    <a href="/dashboard/tokens">Tokens &amp; Mining</a>
    <a href="/dashboard/nodes">Nodes</a>
    <a href="/dashboard/economy">Economy</a>
  </nav>
  <span class="muted mono" style="margin-left:auto;font-size:12px">v{{VERSION}}</span>
</header>`

// ─── /dashboard/tokens ──────────────────────────

const tokensDashboardHTML = commonHead + `<body>` + commonHeader + `
<main>
  <p class="muted" style="margin: 0 0 16px 0">
    Workspace ID required — pass <span class="mono">?ws=&lt;id&gt;</span> in the URL or use the form below.
  </p>
  <section>
    <h2>Workspace</h2>
    <div class="form-row">
      <label>Workspace ID</label>
      <input id="ws-input" type="text" placeholder="ws_..." style="flex:1">
      <button onclick="setWorkspace()">Load</button>
    </div>
  </section>

  <section>
    <h2>🪙 LENS Token Balance</h2>
    <div class="grid">
      <div class="stat">
        <div class="label">Current Balance</div>
        <div class="value lens" id="balance-current">—</div>
      </div>
      <div class="stat">
        <div class="label">Lifetime Earned</div>
        <div class="value lens" id="balance-earned">—</div>
      </div>
      <div class="stat">
        <div class="label">Lifetime Spent</div>
        <div class="value" id="balance-spent">—</div>
      </div>
    </div>
  </section>

  <section>
    <h2>⛏️ Mining Activity</h2>
    <div class="grid" id="mining-grid">
      <div class="stat"><div class="label">Cache</div><div class="value lens" id="m-cache">—</div></div>
      <div class="stat"><div class="label">Compute</div><div class="value lens" id="m-compute">—</div></div>
      <div class="stat"><div class="label">Embeddings</div><div class="value lens" id="m-embedding">—</div></div>
      <div class="stat"><div class="label">Annotations</div><div class="value lens" id="m-annotation">—</div></div>
      <div class="stat"><div class="label">Patterns</div><div class="value lens" id="m-pattern">—</div></div>
    </div>
  </section>

  <section>
    <h2>🔒 Staking</h2>
    <div class="form-row">
      <label>Amount</label>
      <input id="stake-amount" type="number" step="0.01" placeholder="100">
      <label>Lock days</label>
      <select id="stake-days">
        <option value="30">30 days (5% APY)</option>
        <option value="90" selected>90 days (12% APY)</option>
        <option value="180">180 days (20% APY)</option>
      </select>
      <button onclick="stake()">Stake LENS</button>
      <span id="stake-projection" class="muted"></span>
    </div>
    <table id="stakes-table">
      <thead><tr><th>Amount</th><th>Lock</th><th>APY</th><th>Unlocks</th><th>Yield</th><th></th></tr></thead>
      <tbody id="stakes-body"><tr><td colspan="6" class="muted">No positions yet.</td></tr></tbody>
    </table>
  </section>

  <section>
    <h2>🏪 Marketplace</h2>
    <h3 style="font-size:13px;color:var(--secondary);margin-top:0">List LENS for sale</h3>
    <div class="form-row">
      <label>Amount</label>
      <input id="list-amount" type="number" step="0.01" placeholder="100">
      <label>Price (USD)</label>
      <input id="list-price" type="number" step="0.001" placeholder="0.08">
      <button onclick="listForSale()">Create Listing</button>
    </div>
    <h3 style="font-size:13px;color:var(--secondary);margin-top:24px">Active listings</h3>
    <table id="listings-table">
      <thead><tr><th>Seller</th><th>Amount</th><th>Price</th><th></th></tr></thead>
      <tbody id="listings-body"><tr><td colspan="4" class="muted">Loading…</td></tr></tbody>
    </table>
  </section>

  <section>
    <h2>📜 Recent Transactions</h2>
    <table>
      <thead><tr><th>Date</th><th>Type</th><th>Amount</th><th>Balance After</th></tr></thead>
      <tbody id="history-body"><tr><td colspan="4" class="muted">Loading…</td></tr></tbody>
    </table>
  </section>

  <div id="toast" class="toast"></div>
</main>
<footer>Talyvor Lens v{{VERSION}}</footer>
<script>
const LENS_USD = 0.10;
function getWS() {
  const url = new URL(window.location.href);
  return url.searchParams.get('ws') || localStorage.getItem('talyvor_ws') || '';
}
function setWorkspace() {
  const v = document.getElementById('ws-input').value.trim();
  if (!v) return;
  const u = new URL(window.location.href);
  u.searchParams.set('ws', v);
  window.location.href = u.toString();
}
function fmt(n) { return (n || 0).toFixed(2); }
function fmtUSD(n) { return '$' + (n * LENS_USD).toFixed(2); }
function toast(msg, ok) {
  const t = document.getElementById('toast');
  t.textContent = msg;
  t.className = 'toast ' + (ok ? 'good' : 'bad');
  t.style.display = 'block';
  setTimeout(() => t.style.display = 'none', 3500);
}
async function api(path, opts) {
  const res = await fetch(path, opts);
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}
async function loadBalance(ws) {
  try {
    const b = await api('/v1/workspaces/' + ws + '/tokens/balance');
    document.getElementById('balance-current').innerHTML = fmt(b.balance) + ' LENS <span class="usd">' + fmtUSD(b.balance) + '</span>';
    document.getElementById('balance-earned').innerHTML = fmt(b.lifetime_earned) + ' LENS';
    document.getElementById('balance-spent').textContent = fmt(b.lifetime_spent) + ' LENS';
  } catch (e) { console.warn('balance', e); }
}
async function loadMining(ws) {
  const tracks = [
    ['mining/cache',       'm-cache',      'current_balance'],
    ['mining/compute',     'm-compute',    'earned_total'],
    ['mining/embeddings',  'm-embedding',  'total_earned'],
    ['mining/annotations', 'm-annotation', 'earned_tokens'],
    ['mining/patterns',    'm-pattern',    'total_earned'],
  ];
  for (const [path, id, key] of tracks) {
    try {
      const d = await api('/v1/workspaces/' + ws + '/tokens/' + path);
      const v = d[key] || 0;
      document.getElementById(id).innerHTML = fmt(v) + ' <span class="usd">' + fmtUSD(v) + '</span>';
    } catch (e) { /* ignore — track may not be enabled */ }
  }
}
async function loadStakes(ws) {
  try {
    const positions = await api('/v1/workspaces/' + ws + '/tokens/stakes');
    const body = document.getElementById('stakes-body');
    if (!positions || positions.length === 0) {
      body.innerHTML = '<tr><td colspan="6" class="muted">No positions yet.</td></tr>';
      return;
    }
    body.innerHTML = positions.map(p => {
      const unlock = new Date(p.unlocks_at).toLocaleDateString();
      return '<tr><td>' + fmt(p.amount) + ' LENS</td><td>' + p.lock_days + 'd</td>' +
             '<td>' + (p.apy * 100).toFixed(0) + '%</td><td>' + unlock + '</td>' +
             '<td>+' + fmt(p.accrued_yield) + '</td>' +
             '<td><button class="secondary" onclick="unstake(\'' + p.id + '\')">Unstake</button></td></tr>';
    }).join('');
  } catch (e) { console.warn('stakes', e); }
}
async function loadListings() {
  try {
    const listings = await api('/v1/marketplace/listings?limit=20');
    const body = document.getElementById('listings-body');
    if (!listings || listings.length === 0) {
      body.innerHTML = '<tr><td colspan="4" class="muted">No active listings.</td></tr>';
      return;
    }
    body.innerHTML = listings.map(l => (
      '<tr><td>' + l.seller_id + '</td>' +
      '<td>' + fmt(l.amount) + ' LENS</td>' +
      '<td>$' + l.price_usd.toFixed(4) + '/LENS</td>' +
      '<td><button onclick="buy(\'' + l.id + '\', ' + l.price_usd + ')">Buy</button></td></tr>'
    )).join('');
  } catch (e) { console.warn('listings', e); }
}
async function loadHistory(ws) {
  try {
    const entries = await api('/v1/workspaces/' + ws + '/tokens/history?limit=10');
    const body = document.getElementById('history-body');
    if (!entries || entries.length === 0) {
      body.innerHTML = '<tr><td colspan="4" class="muted">No transactions yet.</td></tr>';
      return;
    }
    body.innerHTML = entries.map(e => {
      const date = new Date(e.created_at).toLocaleString();
      const sign = e.amount >= 0 ? '+' : '';
      const cls = e.amount >= 0 ? 'accent' : '';
      return '<tr><td>' + date + '</td><td>' + e.type + '</td>' +
             '<td class="' + cls + '">' + sign + fmt(e.amount) + '</td>' +
             '<td>' + fmt(e.balance_after) + '</td></tr>';
    }).join('');
  } catch (e) { console.warn('history', e); }
}
async function stake() {
  const ws = getWS();
  if (!ws) return toast('Set a workspace ID first', false);
  const amount = parseFloat(document.getElementById('stake-amount').value);
  const lockDays = parseInt(document.getElementById('stake-days').value);
  if (!amount) return toast('Amount required', false);
  try {
    await api('/v1/workspaces/' + ws + '/tokens/stake', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({amount, lock_days: lockDays}),
    });
    toast('Staked ' + amount + ' LENS', true);
    loadBalance(ws); loadStakes(ws);
  } catch (e) { toast('Stake failed: ' + e.message, false); }
}
async function unstake(id) {
  const ws = getWS();
  try {
    await api('/v1/workspaces/' + ws + '/tokens/stake/' + id + '/unstake', {method: 'POST'});
    toast('Unstaked', true);
    loadBalance(ws); loadStakes(ws);
  } catch (e) { toast('Unstake failed: ' + e.message, false); }
}
async function listForSale() {
  const ws = getWS();
  if (!ws) return toast('Set a workspace ID first', false);
  const amount = parseFloat(document.getElementById('list-amount').value);
  const price  = parseFloat(document.getElementById('list-price').value);
  if (!amount || !price) return toast('Amount + price required', false);
  try {
    await api('/v1/marketplace/listings', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({seller_id: ws, amount, price_usd: price}),
    });
    toast('Listing created', true);
    loadListings(); loadBalance(ws);
  } catch (e) { toast('Listing failed: ' + e.message, false); }
}
async function buy(listingID, priceUSD) {
  const ws = getWS();
  if (!ws) return toast('Set a workspace ID first', false);
  const amountUSD = prompt('Amount in USD to spend?');
  if (!amountUSD) return;
  try {
    await api('/v1/marketplace/listings/' + listingID + '/buy', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({buyer_workspace: ws, amount_usd: parseFloat(amountUSD)}),
    });
    toast('Purchase complete', true);
    loadListings(); loadBalance(ws);
  } catch (e) { toast('Buy failed: ' + e.message, false); }
}
function projectYield() {
  const amount = parseFloat(document.getElementById('stake-amount').value) || 0;
  const days = parseInt(document.getElementById('stake-days').value);
  const apy = days === 30 ? 0.05 : days === 90 ? 0.12 : 0.20;
  const yieldLENS = amount * apy * (days / 365);
  document.getElementById('stake-projection').textContent =
    amount ? 'You will earn ' + yieldLENS.toFixed(2) + ' LENS ($' + (yieldLENS * LENS_USD).toFixed(2) + ')' : '';
}
document.addEventListener('DOMContentLoaded', () => {
  const ws = getWS();
  if (ws) {
    document.getElementById('ws-input').value = ws;
    localStorage.setItem('talyvor_ws', ws);
    loadBalance(ws);
    loadMining(ws);
    loadStakes(ws);
    loadHistory(ws);
  }
  loadListings();
  document.getElementById('stake-amount').addEventListener('input', projectYield);
  document.getElementById('stake-days').addEventListener('change', projectYield);
});
</script>
</body></html>`

// ─── /dashboard/nodes ───────────────────────────

const nodesDashboardHTML = commonHead + `<body>` + commonHeader + `
<main>
  <p class="muted" style="margin: 0 0 16px 0">
    Mining nodes registered to a workspace. Pass <span class="mono">?ws=&lt;id&gt;</span> to view your own.
  </p>
  <section>
    <h2>Workspace</h2>
    <div class="form-row">
      <label>Workspace ID</label>
      <input id="ws-input" type="text" placeholder="ws_..." style="flex:1">
      <button onclick="setWorkspace()">Load</button>
    </div>
  </section>

  <section>
    <h2>🖥️ Inference Nodes (GPU)</h2>
    <table>
      <thead><tr><th>ID</th><th>URL</th><th>GPU</th><th>Models</th><th>Status</th></tr></thead>
      <tbody id="inference-body"><tr><td colspan="5" class="muted">Set a workspace ID to view.</td></tr></tbody>
    </table>
  </section>

  <section>
    <h2>📚 Embedding Nodes</h2>
    <table>
      <thead><tr><th>ID</th><th>URL</th><th>Model</th><th>Dims</th><th>Status</th></tr></thead>
      <tbody id="embedding-body"><tr><td colspan="5" class="muted">Set a workspace ID to view.</td></tr></tbody>
    </table>
  </section>
</main>
<footer>Talyvor Lens v{{VERSION}}</footer>
<script>
function getWS() {
  return new URL(window.location.href).searchParams.get('ws')
    || localStorage.getItem('talyvor_ws') || '';
}
function setWorkspace() {
  const v = document.getElementById('ws-input').value.trim();
  if (!v) return;
  const u = new URL(window.location.href);
  u.searchParams.set('ws', v);
  window.location.href = u.toString();
}
async function api(path) {
  const r = await fetch(path);
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}
function statusBadge(active, verified) {
  if (!active) return '<span class="bad">inactive</span>';
  if (!verified) return '<span class="muted">pending verify</span>';
  return '<span class="accent">online ✅</span>';
}
async function loadInference(ws) {
  try {
    const nodes = await api('/v1/workspaces/' + ws + '/nodes');
    const body = document.getElementById('inference-body');
    if (!nodes || nodes.length === 0) {
      body.innerHTML = '<tr><td colspan="5" class="muted">No inference nodes registered.</td></tr>';
      return;
    }
    body.innerHTML = nodes.map(n => (
      '<tr><td>' + n.id + '</td><td>' + n.url + '</td>' +
      '<td>' + n.gpu_type + '</td>' +
      '<td>' + (n.models || []).join(', ') + '</td>' +
      '<td>' + statusBadge(n.active, n.verified) + '</td></tr>'
    )).join('');
  } catch (e) { console.warn(e); }
}
async function loadEmbedding(ws) {
  try {
    const nodes = await api('/v1/workspaces/' + ws + '/embedding-nodes');
    const body = document.getElementById('embedding-body');
    if (!nodes || nodes.length === 0) {
      body.innerHTML = '<tr><td colspan="5" class="muted">No embedding nodes registered.</td></tr>';
      return;
    }
    body.innerHTML = nodes.map(n => (
      '<tr><td>' + n.id + '</td><td>' + n.url + '</td>' +
      '<td>' + n.model + '</td><td>' + n.dimensions + '</td>' +
      '<td>' + statusBadge(n.active, n.verified) + '</td></tr>'
    )).join('');
  } catch (e) { console.warn(e); }
}
document.addEventListener('DOMContentLoaded', () => {
  const ws = getWS();
  if (ws) {
    document.getElementById('ws-input').value = ws;
    localStorage.setItem('talyvor_ws', ws);
    loadInference(ws);
    loadEmbedding(ws);
  }
});
</script>
</body></html>`

// ─── /dashboard/economy ─────────────────────────

const economyDashboardHTML = commonHead + `<body>` + commonHeader + `
<main>
  <section>
    <h2>🌐 Global LENS Economy</h2>
    <div class="grid">
      <div class="stat">
        <div class="label">Total Supply</div>
        <div class="value lens" id="total-supply">—</div>
      </div>
      <div class="stat">
        <div class="label">Circulating</div>
        <div class="value lens" id="circulating">—</div>
      </div>
      <div class="stat">
        <div class="label">Burned</div>
        <div class="value" id="burned">—</div>
      </div>
      <div class="stat">
        <div class="label">Staked</div>
        <div class="value" id="staked">—</div>
      </div>
      <div class="stat">
        <div class="label">Active Listings</div>
        <div class="value" id="listings">—</div>
      </div>
      <div class="stat">
        <div class="label">Avg Listing Price</div>
        <div class="value" id="avg-price">—</div>
      </div>
    </div>
  </section>

  <section>
    <h2>📊 Active Marketplace Listings</h2>
    <table>
      <thead><tr><th>Seller</th><th>Amount</th><th>Price</th><th>Total Value</th></tr></thead>
      <tbody id="listings-body"><tr><td colspan="4" class="muted">Loading…</td></tr></tbody>
    </table>
  </section>

  <section>
    <h2>💰 Earning rate matrix</h2>
    <pre class="mono" id="rates" style="font-size:12px;color:var(--secondary);background:var(--bg);padding:16px;border-radius:6px;overflow:auto">Loading…</pre>
  </section>
</main>
<footer>Talyvor Lens v{{VERSION}}</footer>
<script>
async function api(path) {
  const r = await fetch(path);
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}
function fmt(n) { return (n || 0).toFixed(2); }
function fmtUSD(n) { return '$' + (n * 0.10).toFixed(2); }
async function loadStats() {
  try {
    const s = await api('/v1/economy/stats');
    document.getElementById('total-supply').innerHTML = fmt(s.total_supply) + ' LENS <span class="usd">' + fmtUSD(s.total_supply) + '</span>';
    document.getElementById('circulating').innerHTML = fmt(s.circulating_supply) + ' LENS';
    document.getElementById('burned').textContent = fmt(s.total_burned) + ' LENS';
    document.getElementById('staked').textContent = fmt(s.total_staked) + ' LENS';
    document.getElementById('listings').textContent = s.market_listings;
    document.getElementById('avg-price').textContent = '$' + (s.avg_price_usd || 0).toFixed(4) + '/LENS';
  } catch (e) { console.warn(e); }
}
async function loadListings() {
  try {
    const listings = await api('/v1/marketplace/listings?limit=20');
    const body = document.getElementById('listings-body');
    if (!listings || listings.length === 0) {
      body.innerHTML = '<tr><td colspan="4" class="muted">No active listings.</td></tr>';
      return;
    }
    body.innerHTML = listings.map(l => {
      const total = (l.amount * l.price_usd).toFixed(2);
      return '<tr><td>' + l.seller_id + '</td>' +
             '<td>' + fmt(l.amount) + ' LENS</td>' +
             '<td>$' + l.price_usd.toFixed(4) + '/LENS</td>' +
             '<td>$' + total + '</td></tr>';
    }).join('');
  } catch (e) { console.warn(e); }
}
async function loadRates() {
  try {
    const r = await api('/v1/tokens/rates');
    document.getElementById('rates').textContent = JSON.stringify(r, null, 2);
  } catch (e) { console.warn(e); }
}
document.addEventListener('DOMContentLoaded', () => {
  loadStats();
  loadListings();
  loadRates();
});
</script>
</body></html>`
