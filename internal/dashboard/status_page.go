package dashboard

// statusHTML is the whole page: one card, four rows, no dependencies.
//
// THE DESIGN IS THE SUITE'S, TAKEN FROM ITS SHIPPED CSS — talyvor-suite
// packages/ui/src/theme.css and preset.ts, not from a description of them. Every
// colour below is a verbatim token value; the type scale (head 17/600,
// body 14/1.45, caption 12, micro 12.5/500), the 38px row, the 16px gutter and
// the 10px card radius are the suite's own. The reference is macOS System
// Settings: a dense stack of hairline-separated rows, label left and value
// right, system font, nothing decorative. status_test.go pins the hexes, so a
// drift from the design system fails the build rather than quietly diverging.
//
// THE ONE INVARIANT WORTH NAMING: text is never a hue. The health state is a
// word in ink beside a small coloured dot — the dot carries the status colour,
// the word never does. That is the suite's rule and it is why this page has no
// green "Healthy" or red "Down" label.
//
// It is entirely self-contained: no CDN, no web font, no analytics. The API host
// must not make a visitor's browser talk to a third party.
const statusHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>Talyvor Lens</title>
<style>
:root{
  --sans:-apple-system,BlinkMacSystemFont,"SF Pro Text","Segoe UI",Inter,system-ui,sans-serif;
  --canvas:#F4F5F6; --surface:#FFFFFF; --rule:rgba(0,0,0,.085); --rule-strong:rgba(0,0,0,.14);
  --ink:#1B1D1F; --muted:#6B6E73; --faint:#8B8F94;
  --accent:#0B7A85; --accent-ink:#FFFFFF;
  --settled:#1D7A45; --held:#8A6A12; --slashed:#BF3B2E;
}
@media (prefers-color-scheme: dark){
  :root{
    --canvas:#141618; --surface:#1D2023; --rule:rgba(255,255,255,.085); --rule-strong:rgba(255,255,255,.155);
    --ink:#EDEFF1; --muted:#9CA1A6; --faint:#767B80;
    --accent:#3ABDC9; --accent-ink:#08191B;
    --settled:#45C77F; --held:#D6A93C; --slashed:#F0685C;
  }
}
*{box-sizing:border-box}
html,body{height:100%}
body{
  margin:0; background:var(--canvas); color:var(--ink);
  font-family:var(--sans); font-size:13px; line-height:1.45;
  -webkit-font-smoothing:antialiased; text-rendering:optimizeLegibility;
  display:flex; align-items:flex-start; justify-content:center; padding:48px 16px;
}
main{width:100%; max-width:560px}
.card{background:var(--surface); border:1px solid var(--rule); border-radius:10px; overflow:hidden}
.head{display:flex; align-items:center; gap:12px; padding:16px; border-bottom:1px solid var(--rule)}
.mark{width:26px; height:26px; flex:none; border:1px solid var(--rule-strong); border-radius:7px;
      display:flex; align-items:center; justify-content:center; background:var(--canvas)}
.mark i{display:block; width:3px; height:12px; border-radius:9999px; background:var(--accent)}
.title{flex:1; min-width:0}
h1{margin:0; font-size:17px; line-height:1.3; font-weight:600; color:var(--ink)}
.sub{margin:1px 0 0; font-size:12px; line-height:1.35; color:var(--muted)}
.ver{font-size:12.5px; line-height:1; font-weight:500; color:var(--muted);
     border:1px solid var(--rule); border-radius:9999px; padding:4px 8px; white-space:nowrap}
.row{display:flex; align-items:center; justify-content:space-between; gap:16px;
     min-height:38px; padding:8px 16px; border-bottom:1px solid var(--rule)}
.row:last-child{border-bottom:0}
.label{font-size:14px; color:var(--ink)}
.hint{font-size:12px; line-height:1.35; color:var(--muted)}
.val{font-size:14px; color:var(--ink); display:flex; align-items:center; gap:8px; white-space:nowrap}
.dot{width:6px; height:6px; border-radius:9999px; background:var(--faint); flex:none}
.dot.ok{background:var(--settled)} .dot.warn{background:var(--held)} .dot.bad{background:var(--slashed)}
.note{padding:14px 16px; border-bottom:1px solid var(--rule)}
.note p{margin:0; font-size:14px; color:var(--muted)}
.note p + p{margin-top:8px}
a.row{text-decoration:none; color:inherit}
a.row:hover{background:var(--canvas)}
a.row .go{font-size:14px; color:var(--accent)}
a.row:focus-visible{outline:2px solid var(--accent); outline-offset:-2px}
footer{margin-top:12px; text-align:center; font-size:12px; color:var(--faint)}
@media (prefers-reduced-motion: reduce){
  *,*::before,*::after{animation-duration:.001ms!important; transition-duration:.001ms!important}
}
</style>
</head>
<body>
<main>
  <div class="card">
    <div class="head">
      <span class="mark" aria-hidden="true"><i></i></span>
      <div class="title">
        <h1>Talyvor Lens</h1>
        <p class="sub">Inference gateway</p>
      </div>
      <span class="ver">v{{VERSION}}</span>
    </div>

    <div class="row">
      <div>
        <div class="label">Service</div>
        <div class="hint">Live, read from /healthz</div>
      </div>
      <div class="val" id="health" aria-live="polite"><span class="dot" id="dot"></span><span id="health-word">Checking…</span></div>
    </div>

    <div class="row">
      <div>
        <div class="label">Running for</div>
        <div class="hint">Since this instance last started</div>
      </div>
      <div class="val" id="uptime">—</div>
    </div>

    <div class="note">
      <p>This host answers the Lens API. It is an API, not a user interface — there is
         nothing to sign in to here.</p>
      <p>No account data appears on this page: it holds no credential, by design. Your
         workspace, usage and billing live in the app.</p>
    </div>

    <a class="row" href="https://app.talyvor.com">
      <div>
        <div class="label">Dashboard</div>
        <div class="hint">app.talyvor.com</div>
      </div>
      <span class="go" aria-hidden="true">&rsaquo;</span>
    </a>

    <a class="row" href="https://docs.talyvor.com">
      <div>
        <div class="label">Documentation</div>
        <div class="hint">docs.talyvor.com</div>
      </div>
      <span class="go" aria-hidden="true">&rsaquo;</span>
    </a>
  </div>
  <footer>Talyvor Lens v{{VERSION}}</footer>
</main>
<script>
// The page's only live reading. /healthz is unauthenticated, so this is the one
// thing the page can show truthfully without a credential — and it is fetched,
// never painted in. A failure says so plainly rather than leaving a stale word.
(function () {
  var word = document.getElementById('health-word');
  var dot = document.getElementById('dot');
  var up = document.getElementById('uptime');

  function human(sec) {
    sec = Math.max(0, Math.floor(sec || 0));
    var d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
    if (d) { return d + 'd ' + h + 'h'; }
    if (h) { return h + 'h ' + m + 'm'; }
    return m + 'm';
  }

  function paint(status, uptimeSeconds) {
    // Deliberately the OVERALL word only. The per-check detail (database, cache,
    // replica) is in /healthz for an operator who asks for it; enumerating a
    // host's internals to every passer-by is not this page's job.
    var map = { healthy: 'ok', degraded: 'warn', unhealthy: 'bad' };
    dot.className = 'dot ' + (map[status] || '');
    word.textContent = status ? status.charAt(0).toUpperCase() + status.slice(1) : 'Unknown';
    up.textContent = uptimeSeconds === null ? '—' : human(uptimeSeconds);
  }

  function load() {
    fetch('/healthz', { headers: { 'Accept': 'application/json' }, cache: 'no-store' })
      .then(function (r) { return r.json(); })
      .then(function (d) { paint(String(d.status || ''), d.uptime_seconds); })
      .catch(function () {
        dot.className = 'dot bad';
        word.textContent = 'Unreachable';
        up.textContent = '—';
      });
  }
  load();
  setInterval(load, 30000);
})();
</script>
</body>
</html>
`
