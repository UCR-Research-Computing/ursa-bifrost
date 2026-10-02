/* Ursa Major dashboard (ursa-agent 0.3.0). Read-only panels over bifrost tools + the assistant.
   Every value from the cluster is put on the page with textContent (never innerHTML): job
   names, users, paths and log text are untrusted. SPEC section 20. */
'use strict';

// ---------------------------------------------------------------- helpers
function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = String(text);
  return e;
}
function append(parent, ...kids) { for (const k of kids) if (k) parent.appendChild(k); return parent; }
const $ = (id) => document.getElementById(id);
const usd = (v) => (v === undefined || v === null || Number.isNaN(Number(v))) ? '-' :
  (Number(v) > 0 && Number(v) < 0.01 ? '<$0.01' : '$' + Number(v).toLocaleString(undefined, {minimumFractionDigits: 2, maximumFractionDigits: 2}));
const num = (v, d = 1) => (v === undefined || v === null) ? '-' : Number(v).toLocaleString(undefined, {maximumFractionDigits: d});
function bytes(b) {
  if (b === undefined || b === null) return '-';
  const u = ['B', 'KB', 'MB', 'GB', 'TB']; let i = 0, v = Number(b);
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i ? v.toFixed(v < 10 ? 1 : 0) : v) + ' ' + u[i];
}
function dur(s) {
  s = Number(s || 0); if (!s) return '0s';
  const d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600), m = Math.floor(s % 3600 / 60);
  if (d) return d + 'd ' + h + 'h'; if (h) return h + 'h ' + String(m).padStart(2, '0') + 'm';
  if (m) return m + 'm ' + String(s % 60).padStart(2, '0') + 's'; return s + 's';
}
function ago(iso) {
  if (!iso) return '';
  const t = Date.parse(iso); if (Number.isNaN(t)) return '';
  const s = Math.max(0, (Date.now() - t) / 1000);
  if (s < 90) return 'just now'; if (s < 3600) return Math.round(s / 60) + ' min ago';
  if (s < 86400) return Math.round(s / 3600) + ' h ago'; return Math.round(s / 86400) + ' d ago';
}
function when(iso) {
  if (!iso) return '-'; const d = new Date(iso); if (Number.isNaN(d.getTime())) return '-';
  return d.toLocaleString(undefined, {month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit'});
}
function stateChip(st) { const s = String(st || '').split(' ')[0]; return el('span', 'state ' + s.replace(/[^A-Z_]/g, ''), s || '?'); }
function stat(k, v, cls, sub) { return append(el('div', 'stat'), el('div', 'k', k), el('div', 'v' + (cls ? ' ' + cls : ''), v), sub ? el('div', 's', sub) : null); }
function bar(pct, cls) { const b = el('div', 'bar' + (cls ? ' ' + cls : '')); const i = el('i'); i.style.width = Math.max(0, Math.min(100, pct)) + '%'; b.appendChild(i); return b; }
function table(cols, rows, onRow) {
  const t = el('table', 't'), thead = el('thead'), hr = el('tr');
  for (const c of cols) { const th = el('th', c.num ? 'num' : (c.cls || ''), c.label); if (c.hideSm) th.classList.add('hide-sm'); hr.appendChild(th); }
  thead.appendChild(hr); t.appendChild(thead);
  const tb = el('tbody');
  for (const r of rows) {
    const tr = el('tr');
    for (const c of cols) {
      const v = c.get(r); const td = el('td', c.num ? 'num' : (c.cls || ''));
      if (c.hideSm) td.classList.add('hide-sm');
      if (v instanceof Node) td.appendChild(v); else td.textContent = (v === undefined || v === null || v === '') ? '-' : String(v);
      if (c.title) td.title = c.title(r) || '';
      tr.appendChild(td);
    }
    if (onRow) { tr.className = 'clickable'; tr.tabIndex = 0; tr.onclick = () => onRow(r);
      tr.onkeydown = (e) => { if (e.key === 'Enter') onRow(r); }; }
    tb.appendChild(tr);
  }
  t.appendChild(tb); return t;
}
function quote(untrusted) {
  if (!untrusted || !untrusted.text) return null;
  return append(el('div'), el('div', 'quote-note', 'Text from the cluster (shown as plain text)'), el('div', 'quote', untrusted.text));
}
function notes(list) { if (!(list || []).length) return null; const d = el('div', 'notes'); for (const n of list) d.appendChild(el('div', 'note', n)); return d; }
function setStatus(t) { $('status').textContent = t; }
async function getJSON(url, opts) {
  const r = await fetch(url, Object.assign({credentials: 'same-origin'}, opts || {}));
  let j = {}; try { j = await r.json(); } catch (e) { j = {error: 'HTTP ' + r.status}; }
  if (r.status === 401 && j.login) { location.href = j.login; throw new Error('sign-in expired'); }
  return {ok: r.ok, status: r.status, body: j};
}
async function copyText(t) {
  try { await navigator.clipboard.writeText(t); setStatus('Copied'); }
  catch (e) { const ta = el('textarea'); ta.value = t; document.body.appendChild(ta); ta.select(); document.execCommand('copy'); ta.remove(); setStatus('Copied'); }
}

// ---------------------------------------------------------------- modal
let lastFocus = null;
function openModal(title) {
  lastFocus = document.activeElement;
  $('modal-title').textContent = title; const body = $('modal-body'); body.replaceChildren(el('div', 'skeleton'), el('div', 'skeleton'));
  $('modal').hidden = false; $('modal-close').focus(); return body;
}
function closeModal() { $('modal').hidden = true; if (lastFocus && lastFocus.focus) lastFocus.focus(); }

// ---------------------------------------------------------------- panels
const ME = {staff: false, email: ''};
const PANELS = [];          // {id, title, width, staff, lazy, poll, tools(), url(), render(body,data,resp)}
const STATE = {};           // id -> {opened, loading, seq}

function panel(def) { PANELS.push(def); return def; }

async function loadPanel(p, force) {
  const s = STATE[p.id] || (STATE[p.id] = {seq: 0});
  const seq = ++s.seq; s.opened = true;
  const body = p.body, meta = p.meta;
  if (!body.dataset.filled) body.replaceChildren(el('div', 'skeleton'), el('div', 'skeleton'), el('div', 'skeleton'));
  meta.textContent = 'loading...';
  try {
    const r = await getJSON(p.url() + (force ? (p.url().includes('?') ? '&' : '?') + 'refresh=1' : ''));
    if (seq !== s.seq) return;  // a newer load started (range changed)
    if (!r.ok) { body.replaceChildren(el('div', 'err', r.body.error || ('HTTP ' + r.status))); meta.textContent = ''; body.dataset.filled = ''; return; }
    body.replaceChildren(); body.dataset.filled = '1';
    p.render(body, r.body.data, r.body);
    body.classList.toggle('scrolls', body.scrollHeight > body.clientHeight + 4);
    const age = r.body.cached ? (r.body.refreshing ? ' (refreshing)' : '') : '';
    meta.textContent = (r.body.as_of ? 'as of ' + when(r.body.as_of) : '') + age;
    if (r.body.truncated) body.appendChild(el('div', 'note', 'Showing the first page only.'));
    if (r.body.refreshing) setTimeout(() => { if (seq === s.seq) loadPanel(p, false); }, 8000);
    if (p.after) p.after(r.body.data);
  } catch (e) {
    if (seq !== s.seq) return;
    body.replaceChildren(el('div', 'err', 'Could not load: ' + e.message)); meta.textContent = '';
  }
}

function buildPanel(p) {
  const box = el('section', 'panel ' + (p.width || '') + (p.staff ? ' staff' : ''));
  box.id = 'panel-' + p.id;
  const head = el('div', 'panel-head');
  const title = el('h2', 'panel-title', p.title);
  const tools = el('div', 'panel-tools');
  if (p.tools) for (const t of p.tools()) tools.appendChild(t);
  const ask = el('button', 'btn quiet', 'Ask'); ask.title = 'Ask the assistant about this panel';
  ask.onclick = () => openChat(p.ask ? p.ask() : ('Tell me about my ' + p.title.toLowerCase() + ' on Ursa Major.'));
  const refresh = el('button', 'btn quiet', 'Refresh'); refresh.title = 'Refresh this panel'; refresh.onclick = () => loadPanel(p, true);
  tools.append(ask, refresh);
  const meta = el('span', 'panel-meta');
  append(head, title, meta, tools);
  const body = el('div', 'panel-body');
  p.body = body; p.meta = meta;
  if (p.lazy) {
    const b = el('button', 'btn', p.lazyLabel || 'Load');
    b.onclick = () => loadPanel(p, false);
    body.appendChild(append(el('div', 'lazy'), b, el('span', null, p.lazyNote || 'Loads on request; it reads accounting data on the cluster.')));
  }
  append(box, head, body);
  return box;
}

function rangeSelect(p, options, def) {
  const s = el('select'); s.setAttribute('aria-label', 'Time range');
  for (const [v, label] of options) { const o = el('option', null, label); o.value = v; if (v === def) o.selected = true; s.appendChild(o); }
  p.range = def; s.onchange = () => { p.range = s.value; p.body.dataset.filled = ''; loadPanel(p, false); };
  return s;
}

// ---- Cluster pulse
let PARTS_SHARED = {};
const pulse = panel({
  id: 'pulse', title: 'Cluster pulse', width: 'w12', poll: true,
  url: () => '/api/panel/pulse',
  ask: () => 'Give me a short summary of what is happening on Ursa Major right now.',
  render(body, d) {
    PARTS_SHARED = {};
    const shared = (d.notes || []).find((n) => /^Shared partitions/.test(n)) || '';
    const m = shared.match(/^Shared partitions \(([^)]*)\)/); if (m) for (const n of m[1].split(',')) PARTS_SHARED[n.trim()] = true;
    const stats = el('div', 'stats');
    append(stats,
      stat('Running jobs', num(d.jobs_running, 0), d.jobs_running ? 'cyan' : ''),
      stat('Pending jobs', num(d.jobs_pending, 0), d.jobs_pending ? 'amber' : ''),
      stat('Nodes up', num(d.nodes_powered_up, 0), '', 'of ' + (d.partitions || []).reduce((a, p) => a + (p.nodes_total || 0), 0)),
      stat('Spending now', usd(d.current_usd_per_hour) + '/h', d.current_usd_per_hour ? 'amber' : '', 'est., list price'),
      stat('Problem nodes', num(d.problem_nodes, 0), d.problem_nodes ? 'red' : 'green'),
      stat('Idle, billing', num((d.idle_billing_nodes || []).length, 0), (d.idle_billing_nodes || []).length ? 'amber' : '',
        (d.idle_billing_nodes || []).length ? 'up with no job' : 'none'));
    body.appendChild(stats);
    if ((d.idle_billing_nodes || []).length) body.appendChild(append(el('div', 'alert'), el('b', null, 'IDLE'), el('span', null, 'Powered up with no job, still billing: ' + d.idle_billing_nodes.join(', '))));
    const rows = el('div');
    const hdr = append(el('div', 'partrow dim'), el('span', null, 'partition'), el('span', null, 'nodes powered up'), el('span', 'num', 'run / pend'), el('span', 'num hide-sm', '$/node-h'), el('span', 'num', 'up / total'), el('span', 'use', ''));
    hdr.classList.add('hdr'); rows.appendChild(hdr);
    for (const p of d.partitions || []) {
      const up = p.powered_up || 0, tot = p.nodes_total || 0;
      const name = append(el('span', 'pn'), el('span', null, p.name + (p.default ? '*' : '')), el('br'),
        el('span', 'share' + (PARTS_SHARED[p.name] ? '' : ' whole'), PARTS_SHARED[p.name] ? 'shared nodes' : 'whole nodes'));
      const b = bar(tot ? up / tot * 100 : 0, p.down_or_drained ? 'red' : (p.booting ? 'amber' : ''));
      b.title = up + ' up, ' + (p.allocated || 0) + ' allocated, ' + (p.booting || 0) + ' booting, ' + (p.down_or_drained || 0) + ' down';
      const use = el('span', 'use', p.current_usd_per_hour ? usd(p.current_usd_per_hour) + '/h now' : (p.booting ? p.booting + ' booting' : ''));
      append(rows, append(el('div', 'partrow'), name, b, el('span', 'num', (p.jobs_running || 0) + ' / ' + (p.jobs_pending || 0)),
        el('span', 'num hide-sm', p.usd_per_node_hour !== undefined ? usd(p.usd_per_node_hour) : 'n/a'), el('span', 'num', up + ' / ' + tot), use));
    }
    body.appendChild(rows);
    body.appendChild(el('div', 'legend', '* default partition. Shared: a job gets and pays for only the cores and memory it asks for. Whole: every job gets and pays for whole nodes.'));
    const extra = (d.notes || []).filter((n) => !/^Shared partitions|powered up with no job/.test(n));
    const nn = notes(extra); if (nn) body.appendChild(nn);
  },
  after(d) { telemetry(d); },
});

function telemetry(d) {
  const t = $('telemetry'); t.replaceChildren();
  const chip = (label, val, cls) => append(el('span', 'chip' + (cls ? ' ' + cls : '')), el('span', null, label + ' '), el('b', null, val));
  append(t, chip('running', num(d.jobs_running, 0), d.jobs_running ? 'cyan' : ''), chip('pending', num(d.jobs_pending, 0), d.jobs_pending ? 'amber' : ''),
    chip('nodes up', num(d.nodes_powered_up, 0)), chip('now', usd(d.current_usd_per_hour) + '/h', d.current_usd_per_hour ? 'amber' : ''),
    d.problem_nodes ? chip('problem nodes', d.problem_nodes, 'red') : null);
}

// ---- My jobs
const jobs = panel({
  id: 'jobs', title: 'My jobs', width: 'w8', poll: true,
  tools() { return [rangeSelect(this, [['1d', 'last day'], ['7d', '7 days'], ['30d', '30 days']], '7d')]; },
  url() { return '/api/panel/jobs?range=' + encodeURIComponent(this.range); },
  ask: () => 'Look at my recent jobs on Ursa Major: anything failing or stuck, and what should I fix?',
  render(body, d) {
    const list = Array.isArray(d) ? d : [];
    if (!list.length) { body.appendChild(el('div', 'empty', 'No jobs in this range.')); return; }
    const counts = {}; for (const j of list) { const s = String(j.state || '').split(' ')[0]; counts[s] = (counts[s] || 0) + 1; }
    const chips = el('div', 'form-row');
    for (const [s, n] of Object.entries(counts).sort((a, b) => b[1] - a[1])) chips.appendChild(append(el('span'), stateChip(s), el('span', 'dim', ' ' + n)));
    body.appendChild(chips);
    body.appendChild(table([
      {label: 'job', get: (j) => j.job_id, cls: 'mono'},
      {label: 'name', get: (j) => j.name, cls: 'name', title: (j) => j.name},
      {label: 'state', get: (j) => stateChip(j.state)},
      {label: 'partition', get: (j) => j.partition, hideSm: true},
      {label: 'cores', get: (j) => j.cpus, num: true, hideSm: true},
      {label: 'elapsed', get: (j) => dur(j.elapsed_s), num: true},
      {label: 'est. cost', get: (j) => j.est_cost_usd ? usd(j.est_cost_usd) : '-', num: true},
      {label: 'ended', get: (j) => j.ended ? ago(j.ended) : (j.reason || ''), hideSm: true},
    ], list, (j) => showJob(j.job_id, false)));
  },
});

async function showJob(id, any) {
  const body = openModal('Job ' + id);
  const show = await getJSON('/api/panel/job?job=' + encodeURIComponent(id));
  body.replaceChildren();
  if (!show.ok) { body.appendChild(el('div', 'err', show.body.error || 'error')); return; }
  const j = show.body.data || {};
  const kv = el('div', 'kv');
  const add = (k, v) => { if (v === undefined || v === null || v === '') return; append(kv, el('div', 'k', k), v instanceof Node ? append(el('div', 'v'), v) : el('div', 'v', v)); };
  add('name', j.name); add('state', stateChip(j.state)); add('exit code', j.exit_code); add('partition', j.partition);
  add('nodes', (j.node_count || '') + (j.nodes ? '  (' + j.nodes + ')' : '')); add('cores', j.cpus);
  add('time limit', j.time_limit_min ? j.time_limit_min + ' min' : ''); add('elapsed', dur(j.elapsed_s));
  add('submitted', when(j.submitted)); add('started', when(j.started)); add('ended', when(j.ended));
  if (j.est_cost_usd) add('est. cost', usd(j.est_cost_usd));
  if (j.efficiency) {
    const e = j.efficiency;
    add('CPU efficiency', (e.cpu_percent !== undefined ? e.cpu_percent + '%' : '-') + (e.core_seconds_allocated ? ' of ' + num(e.core_seconds_allocated / 3600, 2) + ' core-h' : ''));
    add('memory', (e.mem_peak_mb !== undefined ? num(e.mem_peak_mb, 0) + ' MB peak' : '') + (e.mem_alloc_mb ? ' of ' + num(e.mem_alloc_mb, 0) + ' MB' : ''));
  }
  if (j.requested_tres) add('asked for', Object.entries(j.requested_tres).map(([k, v]) => k + '=' + v).join(', '));
  if (j.allocated_tres) add('got', Object.entries(j.allocated_tres).map(([k, v]) => k + '=' + v).join(', '));
  add('folder', j.work_dir); add('log', j.stdout);
  body.appendChild(kv);
  const sub = quote(j.submit_line_untrusted); if (sub) { body.appendChild(el('h4', null, 'Submitted with')); body.appendChild(sub); }
  const st = String(j.state || '');
  if (/FAILED|TIMEOUT|OUT_OF_MEMORY|NODE_FAIL|BOOT_FAIL|CANCELLED|PREEMPTED/.test(st) || (j.exit_code && j.exit_code !== '0')) {
    body.appendChild(el('h4', null, 'Why it ended this way'));
    const box = append(el('div'), el('div', 'skeleton')); body.appendChild(box);
    const ex = await getJSON('/api/panel/explain?job=' + encodeURIComponent(id));
    box.replaceChildren();
    if (!ex.ok) { box.appendChild(el('div', 'err', ex.body.error || 'error')); }
    else {
      const d = ex.body.data || {};
      if (!(d.findings || []).length) box.appendChild(el('div', 'dim', 'No known cause matched.'));
      for (const f of d.findings || []) {
        append(box, append(el('div', 'finding ' + (f.severity || '')), el('div', 'ft', f.title || f.rule),
          (f.evidence || []).length ? el('div', 'fe', f.evidence.join('\n')) : null, el('div', null, f.suggestion || '')));
      }
      const tail = quote(d.log_tail_untrusted); if (tail) { box.appendChild(el('h4', null, 'End of the log')); box.appendChild(tail); }
    }
  }
  const askb = el('button', 'btn primary', 'Ask the assistant about this job');
  askb.onclick = () => { closeModal(); openChat('Explain job ' + id + ' and tell me what to do next.'); };
  body.appendChild(append(el('div', 'form-row spaced'), askb));
}

// ---- My month (usage)
panel({
  id: 'usage', title: 'My usage', width: 'w4',
  tools() {
    const s = rangeSelect(this, [['7d', '7 days'], ['30d', '30 days']], '30d');
    const by = el('select'); by.setAttribute('aria-label', 'Group by');
    for (const v of ['partition', 'state']) { const o = el('option', null, 'by ' + v); o.value = v; by.appendChild(o); }
    this.by = 'partition'; by.onchange = () => { this.by = by.value; this.body.dataset.filled = ''; loadPanel(this, false); };
    return [s, by];
  },
  url() { return '/api/panel/usage?range=' + this.range + '&by=' + this.by; },
  ask: () => 'Summarise my Ursa Major usage and cost this month, and how I could spend less.',
  render(body, d) {
    const t = d.total || {};
    append(body, append(el('div', 'stats'),
      stat('Est. cost', usd(t.est_cost_usd), 'amber'), stat('Jobs', num(t.jobs, 0), '', (t.failed || 0) + ' failed'),
      stat('Node-hours', num(t.node_hours, 1), '', num(t.core_hours, 0) + ' core-h'),
      stat('CPU efficiency', t.cpu_efficiency_percent !== undefined ? num(t.cpu_efficiency_percent, 0) + '%' : '-',
        t.cpu_efficiency_percent < 30 ? 'red' : (t.cpu_efficiency_percent < 60 ? 'amber' : 'green'))));
    const rows = (d.rows || []).slice().sort((a, b) => (b.est_cost_usd || 0) - (a.est_cost_usd || 0));
    const max = Math.max(...rows.map((r) => r.est_cost_usd || 0), 0.01);
    body.appendChild(table([
      {label: d.group_by || 'key', get: (r) => r.key, cls: 'mono'},
      {label: 'jobs', get: (r) => r.jobs, num: true},
      {label: 'est. cost', get: (r) => usd(r.est_cost_usd), num: true},
      {label: '', get: (r) => bar((r.est_cost_usd || 0) / max * 100, 'amber')},
    ], rows));
    append(body, notes(d.notes));
  },
});

// ---- Waste
panel({
  id: 'waste', title: 'Money left on the table', width: 'w8', lazy: true, lazyLabel: 'Check my jobs',
  lazyNote: 'Looks for idle, failed-fast and timed-out jobs in your accounting.',
  tools() { return [rangeSelect(this, [['7d', '7 days'], ['30d', '30 days']], '7d')]; },
  url() { return '/api/panel/waste?range=' + this.range; },
  ask: () => 'Go through my waste report on Ursa Major and tell me the top things to change.',
  render(body, d) {
    append(body, append(el('div', 'stats'),
      stat('Est. wasted', usd(d.total_est_wasted_usd), d.total_est_wasted_usd ? 'red' : 'green'),
      stat('Wasted node-h', num(d.total_wasted_node_hours, 1)), stat('Jobs scanned', num(d.jobs_scanned, 0))));
    const kinds = Object.entries(d.count_by_kind || {});
    if (kinds.length) { const r = el('div', 'form-row'); for (const [k, n] of kinds) r.appendChild(el('span', 'chip amber', k + ' ' + n)); body.appendChild(r); }
    const items = (d.items || []).slice(0, 8);
    if (!items.length) body.appendChild(el('div', 'empty', 'Nothing wasted above the thresholds. Nice.'));
    for (const it of items) {
      // node-level items (idle-node) have no job and no user
      const what = it.job_id ? 'job ' + it.job_id : 'idle node';
      const head = append(el('div', 'ft'), el('span', null, usd(it.est_wasted_usd) + '  ' + what + '  '), el('span', 'dim', it.kind + ', ' + (it.partition || '')));
      let open = null;
      if (it.job_id) { open = el('button', 'row-btn', 'open job'); open.onclick = () => showJob(it.job_id, false); }
      append(body, append(el('div', 'finding'), head, el('div', 'fe', it.detail || ''), el('div', null, it.suggestion || ''), open));
    }
    append(body, notes(d.notes));
  },
});

// ---- Storage
panel({
  id: 'storage', title: 'My storage', width: 'w4', lazy: true, lazyLabel: 'Measure my folders',
  lazyNote: 'Sizes your home and scratch folders (can take a minute the first time).',
  url: () => '/api/panel/storage',
  ask: () => 'Look at my storage on Ursa Major and suggest what I can clean up.',
  render(body, d) {
    append(body, append(el('div', 'stats'), stat('Home', bytes(d.home_bytes), 'cyan'), stat('Scratch', bytes(d.scratch_bytes))));
    for (const f of d.filesystems || []) {
      const pct = f.used_pct || 0;
      append(body, append(el('div', 'partrow'), el('span', 'pn', f.mount), bar(pct, pct > 85 ? 'red' : (pct > 70 ? 'amber' : '')),
        el('span', 'num', num(pct, 1) + '%'), el('span', 'num hide-sm', bytes(f.free_bytes) + ' free'), el('span')));
    }
    if ((d.largest || []).length) {
      body.appendChild(el('h4', 'dim', 'Largest folders'));
      body.appendChild(table([{label: 'folder', get: (r) => String(r.path || '').replace(/^\/home\/[^/]+\//, '~/'), cls: 'name mono', title: (r) => r.path},
        {label: 'size', get: (r) => bytes(r.bytes), num: true}], d.largest.slice(0, 10)));
    }
    append(body, notes(d.notes));
  },
});

// ---- Partitions + interactive helper
panel({
  id: 'partitions', title: 'Partitions and prices', width: 'w8',
  url: () => '/api/panel/partitions',
  ask: () => 'Which Ursa Major partition should I use for my work, and what will it cost?',
  render(body, d) {
    const list = Array.isArray(d) ? d : [];
    body.appendChild(table([
      {label: 'partition', get: (p) => append(el('span', 'mono'), el('span', null, p.name + (p.default ? '*' : '')), el('br'), el('span', 'share' + (PARTS_SHARED[p.name] ? '' : ' whole'), PARTS_SHARED[p.name] ? 'shared' : 'whole node'))},
      {label: 'cores', get: (p) => p.cpus_per_node, num: true},
      {label: 'memory', get: (p) => p.mem_gb_per_node ? p.mem_gb_per_node + ' GB' : '-', num: true},
      {label: 'GPU', get: (p) => p.gpus_per_node ? p.gpus_per_node + ' x ' + (p.gpu_type || 'GPU') : '-', hideSm: true},
      {label: '$/node-h', get: (p) => p.usd_per_node_hour !== null && p.usd_per_node_hour !== undefined ? usd(p.usd_per_node_hour) : 'n/a', num: true},
      {label: 'use for', get: (p) => p.use_for, hideSm: true},
    ], list));
    // interactive session helper
    body.appendChild(el('h4', 'dim', 'Interactive session: the exact command and its cost'));
    const row = el('div', 'form-row');
    const sel = el('select'); sel.setAttribute('aria-label', 'Partition');
    for (const p of list) { const o = el('option', null, p.name); o.value = p.name; if (p.default) o.selected = true; sel.appendChild(o); }
    const mk = (label, attrs) => { const i = el('input'); Object.assign(i, attrs); i.setAttribute('aria-label', label); return append(el('label', null, label), i); };
    const cpus = mk('cores', {type: 'number', min: 1, max: 64, value: 4});
    const time = mk('time', {type: 'text', value: '1:00:00', size: 8});
    const go = el('button', 'btn', 'Show command');
    append(row, append(el('label', null, 'partition'), sel), cpus, time, go);
    const out = el('div'); append(body, row, out);
    go.onclick = async () => {
      out.replaceChildren(el('div', 'skeleton'));
      const q = new URLSearchParams({partition: sel.value, cpus: cpus.querySelector('input').value, time: time.querySelector('input').value});
      const r = await getJSON('/api/panel/interactive?' + q);
      out.replaceChildren();
      if (!r.ok) { out.appendChild(el('div', 'err', r.body.error)); return; }
      const h = r.body.data || {};
      for (const c of [h.command, h.alternative].filter(Boolean)) {
        const cp = el('button', 'btn ghost', 'Copy'); cp.onclick = () => copyText(c);
        out.appendChild(append(el('div', 'cmd'), el('code', null, c), cp));
      }
      out.appendChild(el('div', null, 'Worst case ' + usd(h.worst_case_usd) + ' at ' + usd(h.usd_per_hour) + '/h for ' + (h.time_limit || '')));
      for (const w of h.warnings || []) out.appendChild(el('div', 'finding', w));
      if (h.connect) out.appendChild(el('div', 'note', 'Connect first: ' + h.connect));
    };
  },
});

// ---- Software finder
panel({
  id: 'modules', title: 'Software finder', width: 'w4',
  tools() {
    const i = el('input'); i.type = 'search'; i.placeholder = 'gromacs, python, cuda...'; i.setAttribute('aria-label', 'Search software'); i.maxLength = 64;
    this.q = ''; i.onkeydown = (e) => { if (e.key === 'Enter') { this.q = i.value.trim(); this.body.dataset.filled = ''; loadPanel(this, false); } };
    return [i];
  },
  lazy: true, lazyLabel: 'List software', lazyNote: 'Search the module catalog (press Enter in the box).',
  url() { return '/api/panel/modules?q=' + encodeURIComponent(this.q || ''); },
  ask() { return 'How do I use ' + (this.q || 'the software stack') + ' on Ursa Major?'; },
  render(body, d) {
    const ms = (d.matches || []);
    if (!ms.length) body.appendChild(el('div', 'empty', 'No matching module.'));
    const box = el('div', 'mods');
    for (const m of ms.slice(0, 40)) {
      append(box, append(el('div', 'mod'), append(el('div'), el('span', 'mn', m.name), el('span', 'dim', '  ' + (m.versions || []).join(', ')), m.gpu ? el('span', 'chip cyan', 'GPU') : null),
        m.requires ? el('div', 'note', 'needs: ' + m.requires) : null, m.usage_card ? el('div', null, m.usage_card) : null));
    }
    body.appendChild(box);
    if ((d.recipes || []).length) {
      body.appendChild(el('h4', 'dim', 'Tested recipes'));
      for (const r of d.recipes.slice(0, 5)) {
        const line = (r.load || []).map((x) => 'module load ' + x).join(' && ') + (r.run ? '  then  ' + r.run : '');
        append(body, append(el('div', 'mod'), el('div', 'mn', r.name), el('div', 'note', (r.field || '') + (r.partition ? ', ' + r.partition : '')), el('div', 'mono', line)));
      }
    }
    if (d.how_to_load) body.appendChild(el('div', 'note', d.how_to_load));
  },
});

// ---- Staged files
panel({
  id: 'uploads', title: 'Staged input files', width: 'w4',
  url: () => '/api/panel/uploads',
  render(body, d) {
    const ups = d.uploads || [];
    body.appendChild(el('div', 'dim', bytes(d.total_bytes) + ' of ' + bytes(d.limit_bytes) + ' used'));
    if (!ups.length) { body.appendChild(el('div', 'empty', 'No staged files. Attach one in the assistant to use it as a job input.')); return; }
    body.appendChild(table([{label: 'file', get: (u) => u.filename, cls: 'name'}, {label: 'size', get: (u) => bytes(u.bytes), num: true},
      {label: 'deleted', get: (u) => when(u.deleted_after)}, {label: 'id', get: (u) => u.upload_id, cls: 'mono hide-sm'}], ups));
  },
});

// ---- Staff panels
panel({
  id: 'health', title: 'Cluster health', width: 'w4', staff: true,
  url: () => '/api/panel/health',
  ask: () => 'Check Ursa Major health and tell me anything that needs attention.',
  render(body, d) {
    append(body, append(el('div', 'stats three'), stat('Status', d.ok ? 'OK' : 'Issues', d.ok ? 'green' : 'red'),
      stat('Failure rate 24h', d.failure_rate_24h_percent !== undefined ? num(d.failure_rate_24h_percent, 1) + '%' : '-', d.failure_rate_24h_percent > 25 ? 'red' : ''),
      stat('Jobs ended 24h', num(d.jobs_ended_24h, 0))));
    for (const i of d.issues || []) append(body, append(el('div', 'finding ' + (i.severity || '')), el('div', 'ft', i.title || i.kind || 'issue'), el('div', 'fe', i.detail || i.message || ''), el('div', null, i.suggestion || '')));
    if (!(d.issues || []).length) body.appendChild(el('div', 'empty', 'No problem nodes or stuck jobs.'));
    append(body, notes(d.notes));
  },
});
panel({
  id: 'allwaste', title: 'Waste, all users', width: 'w4', staff: true, lazy: true, lazyLabel: 'Load waste report',
  tools() { return [rangeSelect(this, [['7d', '7 days'], ['30d', '30 days']], '7d')]; },
  url() { return '/api/panel/allwaste?range=' + this.range; },
  render(body, d) {
    append(body, append(el('div', 'stats'), stat('Est. wasted', usd(d.total_est_wasted_usd), 'red'), stat('Jobs scanned', num(d.jobs_scanned, 0))));
    const by = {};
    for (const it of d.items || []) { const u = it.user || 'idle nodes (no job)'; by[u] = (by[u] || 0) + (it.est_wasted_usd || 0); }
    body.appendChild(table([{label: 'user', get: (r) => r[0], cls: 'mono name'}, {label: 'est. wasted', get: (r) => usd(r[1]), num: true}],
      Object.entries(by).sort((a, b) => b[1] - a[1]).slice(0, 15)));
  },
});

panel({
  id: 'alljobs', title: 'All jobs', width: 'w12', staff: true, lazy: true, lazyLabel: 'Load all jobs',
  tools() { return [rangeSelect(this, [['1d', 'last day'], ['7d', '7 days']], '1d')]; },
  url() { return '/api/panel/alljobs?range=' + this.range; },
  render(body, d) {
    const list = Array.isArray(d) ? d : [];
    if (!list.length) { body.appendChild(el('div', 'empty', 'No jobs in this range.')); return; }
    body.appendChild(table([
      {label: 'job', get: (j) => j.job_id, cls: 'mono'}, {label: 'user', get: (j) => j.user, cls: 'mono name'},
      {label: 'name', get: (j) => j.name, cls: 'name hide-sm'}, {label: 'state', get: (j) => stateChip(j.state)},
      {label: 'partition', get: (j) => j.partition, hideSm: true}, {label: 'elapsed', get: (j) => dur(j.elapsed_s), num: true},
      {label: 'est. cost', get: (j) => j.est_cost_usd ? usd(j.est_cost_usd) : '-', num: true},
    ], list));
  },
});
panel({
  id: 'usersusage', title: 'Usage by user', width: 'w12', staff: true, lazy: true, lazyLabel: 'Load usage by user',
  tools() { return [rangeSelect(this, [['7d', '7 days'], ['30d', '30 days']], '30d')]; },
  url() { return '/api/panel/usersusage?range=' + this.range; },
  render(body, d) {
    const t = d.total || {};
    append(body, append(el('div', 'stats'), stat('Est. cost, all users', usd(t.est_cost_usd), 'amber'), stat('Jobs', num(t.jobs, 0), '', (t.failed || 0) + ' failed'),
      stat('CPU efficiency', t.cpu_efficiency_percent !== undefined ? num(t.cpu_efficiency_percent, 0) + '%' : '-')));
    const rows = (d.rows || []).slice().sort((a, b) => (b.est_cost_usd || 0) - (a.est_cost_usd || 0));
    body.appendChild(table([{label: 'user', get: (r) => r.key, cls: 'mono name'}, {label: 'jobs', get: (r) => r.jobs, num: true},
      {label: 'failed', get: (r) => r.failed, num: true}, {label: 'node-h', get: (r) => num(r.node_hours, 1), num: true, hideSm: true},
      {label: 'CPU eff.', get: (r) => r.cpu_efficiency_percent !== undefined ? num(r.cpu_efficiency_percent, 0) + '%' : '-', num: true},
      {label: 'est. cost', get: (r) => usd(r.est_cost_usd), num: true}], rows));
  },
});
// ---------------------------------------------------------------- polling (visible tab only)
let pollTimer = null;
function startPolling() {
  stopPolling();
  pollTimer = setInterval(() => {
    if (document.hidden) return;
    for (const p of PANELS) if (p.poll && (STATE[p.id] || {}).opened && document.body.contains(p.body)) loadPanel(p, false);
  }, 60000);
}
function stopPolling() { if (pollTimer) clearInterval(pollTimer); pollTimer = null; }
document.addEventListener('visibilitychange', () => { if (!document.hidden && pollTimer) { for (const p of PANELS) if (p.poll && (STATE[p.id] || {}).opened) loadPanel(p, false); } });

// ---------------------------------------------------------------- assistant (chat drawer)
const log = () => $('log');
function scrollLog() { const l = log(); l.scrollTop = l.scrollHeight; }
function linkify(m, text) {
  const re = /https:\/\/storage\.googleapis\.com\/[^\s"'<>)\]]+/g; let last = 0, x;
  while ((x = re.exec(text)) !== null) {
    m.appendChild(document.createTextNode(text.slice(last, x.index)));
    const a = el('a', null, x[0].length > 90 ? x[0].slice(0, 70) + '...' : x[0]); a.href = x[0]; a.target = '_blank'; a.rel = 'noopener noreferrer';
    m.appendChild(a); last = x.index + x[0].length;
  }
  m.appendChild(document.createTextNode(text.slice(last)));
}
function say(cls, text, tools) {
  const m = el('div', 'msg ' + cls); linkify(m, text || '');
  if (tools && tools.length) m.appendChild(el('div', 'tools', 'tools: ' + tools.join(', ')));
  log().appendChild(m); scrollLog();
}
const shownPlans = new Set();
function plans(list) {
  for (const p of (list || [])) {
    if (shownPlans.has(p.id)) continue; shownPlans.add(p.id);
    const s = p.summary || {}, box = el('div', 'plan');
    box.appendChild(el('h3', null, 'Approve this ' + (s.action || p.tool) + '?'));
    const t = el('table');
    const rows = [['job', s.job_id || s.job_name], ['partition', s.partition], ['nodes', s.nodes], ['time limit', s.time_limit],
      ['worst case', s.worst_case_usd !== undefined ? usd(s.worst_case_usd) : undefined],
      ['spent today', s.committed_today_usd !== undefined ? usd(s.committed_today_usd) + ' of ' + usd(s.day_cap_usd) : undefined],
      ['scheduler', s.scheduler_test], ['effect', s.effect], ['folder', s.remote_dir], ['warnings', (s.warnings || []).join('; ') || undefined], ['expires', p.expires_at]];
    for (const [k, v] of rows) { if (v === undefined || v === null || v === '') continue; const r = el('tr'); append(r, el('td', null, k), el('td', null, String(v))); t.appendChild(r); }
    box.appendChild(t);
    const btns = el('div', 'btns'), ok = el('button', 'btn approve', 'Approve'), no = el('button', 'btn reject', 'Reject');
    append(btns, ok, no); box.appendChild(btns);
    const decide = async (d) => {
      ok.disabled = no.disabled = true;
      const r = await getJSON('/api/decide', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({id: p.id, decision: d})});
      const j = r.body; btns.remove();
      box.appendChild(el('div', 'done', d === 'approve' ? (j.decision && j.decision.status === 'done' ? 'Approved and done.' : 'Approved, but bifrost refused: see below.') : 'Rejected. Nothing was done.'));
      if (j.reply) say('bot', j.reply, j.tools);
      plans(j.pending);
      if (d === 'approve') { for (const pp of [jobs, pulse]) loadPanel(pp, true); }
    };
    ok.onclick = () => decide('approve'); no.onclick = () => decide('reject');
    log().appendChild(box); scrollLog();
  }
}
let greeted = false;
function openChat(prefill) {
  $('drawer').hidden = false;
  if (!greeted) { greeted = true; say('bot', 'Hi. Ask me about Ursa Major: running jobs, failures, partitions, modules, costs. I can plan a job for you; nothing runs until you press Approve.'); }
  if (prefill) { $('q').value = prefill; }
  $('q').focus();
}
function closeChat() { $('drawer').hidden = true; $('chatbtn').focus(); }

async function sendChat(e) {
  e.preventDefault(); const q = $('q'), send = $('send'); const text = q.value.trim(); if (!text) return;
  say('you', text); q.value = ''; send.disabled = true; send.textContent = '...'; setStatus('Assistant is working...');
  try {
    const r = await getJSON('/api/chat', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({message: text})});
    if (r.body.error) { say('bot', 'Error: ' + r.body.error); return; }
    say('bot', r.body.reply || '(no reply)', r.body.tools); plans(r.body.pending);
  } catch (err) { say('bot', 'Network error: ' + err.message); }
  finally { send.disabled = false; send.textContent = 'Send'; q.focus(); setStatus('Ready'); }
}
async function attach(e) {
  const f = e.target.files[0]; e.target.value = ''; if (!f) return;
  say('you', 'Attaching ' + f.name + ' (' + bytes(f.size) + ')...');
  try {
    const r = await getJSON('/api/upload', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({filename: f.name, bytes: f.size})});
    const t = r.body;
    if (!r.ok || t.error) { say('bot', 'Upload refused: ' + (t.error || r.status)); return; }
    const put = await fetch(t.upload_url, {method: t.method || 'PUT', headers: t.headers || {}, body: f});
    if (!put.ok) { say('bot', 'Upload to storage failed: HTTP ' + put.status); return; }
    const q = $('q'); q.value = (q.value ? q.value + '\n' : '') + 'I attached ' + t.filename + ' (upload id ' + t.upload_id + ') as an input for my job.';
    say('bot', 'Uploaded ' + t.filename + '. Its upload id is in your message box; say what the job should do with it.');
    const up = PANELS.find((p) => p.id === 'uploads'); if (up) loadPanel(up, true);
    q.focus();
  } catch (err) { say('bot', 'Upload error: ' + err.message); }
}

// ---------------------------------------------------------------- boot
async function boot() {
  const r = await getJSON('/api/me');
  const j = r.body;
  if (!j.signed_in) { $('signin').hidden = false; return; }
  ME.staff = !!j.staff; ME.email = j.email;
  $('who').textContent = j.email; $('actions').hidden = false; $('staffchip').hidden = !ME.staff;
  $('ver').textContent = 'ursa-agent ' + (j.version || '');
  const grid = $('grid'); grid.hidden = false;
  for (const p of PANELS) { if (p.staff && !ME.staff) continue; grid.appendChild(buildPanel(p)); }
  for (const p of PANELS) { if ((p.staff && !ME.staff) || p.lazy) continue; loadPanel(p, false); }
  startPolling();
  if ((j.pending || []).length) { openChat(); plans(j.pending); }
}

document.addEventListener('DOMContentLoaded', () => {
  $('f').addEventListener('submit', sendChat);
  $('q').addEventListener('keydown', (e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); $('f').requestSubmit(); } });
  $('file').addEventListener('change', attach);
  $('chatbtn').addEventListener('click', () => ($('drawer').hidden ? openChat() : closeChat()));
  $('closechat').addEventListener('click', closeChat);
  $('newchat').addEventListener('click', async () => { await getJSON('/api/new', {method: 'POST'}); log().replaceChildren(); shownPlans.clear(); greeted = false; openChat(); });
  $('refreshall').addEventListener('click', () => { for (const p of PANELS) if ((STATE[p.id] || {}).opened && document.body.contains(p.body)) loadPanel(p, true); });
  $('modal-close').addEventListener('click', closeModal);
  $('modal').addEventListener('click', (e) => { if (e.target === $('modal')) closeModal(); });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') { if (!$('modal').hidden) closeModal(); else if (!$('drawer').hidden) closeChat(); return; }
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    const tag = (document.activeElement && document.activeElement.tagName) || '';
    if (/INPUT|TEXTAREA|SELECT/.test(tag) || !$('modal').hidden) return;
    if (e.key === '/' && $('actions').hidden === false) { e.preventDefault(); openChat(); }
  });
  boot().catch((e) => setStatus('Could not start: ' + e.message));
});
