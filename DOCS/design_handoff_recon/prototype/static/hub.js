/* Recon Hub — shared data & helpers
   Fixture: k8s_cert_expired (kubelet serving cert) */

window.Recon = (function () {
  // Small SVG icon helpers (lucide-style)
  const I = (name) => {
    const P = {
      server:   '<path d="M2 4h20v6H2zM2 14h20v6H2zM6 7h.01M6 17h.01"/>',
      puzzle:   '<path d="M19.4 13a2 2 0 0 0 0-4 2 2 0 0 0-2-2V5a2 2 0 0 0-2-2h-2a2 2 0 0 0-2 2v.6a2 2 0 1 1 0 4V11h-.6a2 2 0 1 0 0 4H11v2a2 2 0 0 0 2 2h2a2 2 0 0 0 2-2v-2h.4a2 2 0 0 0 2-2z"/>',
      play:     '<path d="M5 3l14 9-14 9z"/>',
      search:   '<circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/>',
      scroll:   '<path d="M6 3h12v18l-4-2-4 2-4-2zM10 8h4M10 12h4M10 16h4"/>',
      gear:     '<circle cx="12" cy="12" r="3"/><path d="M19 12a7 7 0 0 0-.2-1.7l2-1.5-2-3.4-2.3.9a7 7 0 0 0-3-1.7L13 2h-2l-.5 2.6a7 7 0 0 0-3 1.7l-2.3-1-2 3.5 2 1.5A7 7 0 0 0 5 12a7 7 0 0 0 .2 1.7l-2 1.5 2 3.4 2.3-.9a7 7 0 0 0 3 1.7L11 22h2l.5-2.6a7 7 0 0 0 3-1.7l2.3 1 2-3.5-2-1.5c.1-.5.2-1.1.2-1.7z"/>',
      activity: '<path d="M3 12h4l3-9 4 18 3-9h4"/>',
      clock:    '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
      check:    '<path d="M4 12l5 5 11-11"/>',
      x:        '<path d="M6 6l12 12M6 18L18 6"/>',
      pause:    '<path d="M7 5v14M17 5v14"/>',
      pin:      '<path d="M12 2v8M8 10h8l-2 4H10zM12 14v8"/>',
      ban:      '<circle cx="12" cy="12" r="9"/><path d="m5 5 14 14"/>',
      edit:     '<path d="M12 20h9M16.5 3.5 20 7 8 19l-4 1 1-4z"/>',
      send:     '<path d="M22 2 11 13M22 2l-7 20-4-9-9-4z"/>',
      copy:     '<rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V6a2 2 0 0 1 2-2h9"/>',
      flash:    '<path d="M13 2 3 14h7l-2 8 11-13h-7z"/>',
      layers:   '<path d="M12 3 2 8l10 5 10-5zM2 14l10 5 10-5M2 18l10 5 10-5"/>',
      refresh:  '<path d="M21 12a9 9 0 1 1-3-6.7L21 8M21 3v5h-5"/>',
      caret:    '<path d="m9 6 6 6-6 6"/>',
      dots:     '<circle cx="5" cy="12" r="1.5"/><circle cx="12" cy="12" r="1.5"/><circle cx="19" cy="12" r="1.5"/>',
      warn:     '<path d="m12 3 10 18H2zM12 10v5M12 18v.01"/>',
      terminal: '<path d="M4 17l6-6-6-6M12 19h8"/>',
      user:     '<circle cx="12" cy="8" r="4"/><path d="M4 21a8 8 0 0 1 16 0"/>',
      logout:   '<path d="M15 3h4a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-4M10 17l5-5-5-5M15 12H3"/>',
    };
    return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">${P[name] || ''}</svg>`;
  };

  // Brand mark — round dashed compass with crosshair lines (reconnaissance feel)
  const BRAND = `
  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round">
    <circle cx="12" cy="12" r="9" stroke-dasharray="2 2" opacity="0.55"/>
    <circle cx="12" cy="12" r="5"/>
    <path d="M12 3v4M12 17v4M3 12h4M17 12h4"/>
    <circle cx="12" cy="12" r="1.3" fill="currentColor" stroke="none"/>
  </svg>`;

  // Sidebar config
  const NAV = [
    { key: 'hosts',         label: 'Hosts',         icon: 'server',   href: 'hosts.html',           count: 5 },
    { key: 'collectors',    label: 'Collectors',    icon: 'puzzle',   href: 'collectors.html',      count: 10 },
    { key: 'runs',          label: 'Runs',          icon: 'play',     href: 'runs.html',            count: null },
    { key: 'investigations',label: 'Investigations',icon: 'search',   href: 'investigations.html',  count: 3, accent: true },
    { key: 'audit',         label: 'Audit',         icon: 'scroll',   href: 'audit.html',           count: null },
    { key: 'settings',      label: 'Settings',      icon: 'gear',     href: 'settings.html',        count: null },
  ];

  function renderSidebar(activeKey) {
    return `
    <aside class="sidebar">
      <div class="sidebar-brand">
        <span class="brand-mark" style="color:var(--accent)">${BRAND}</span>
        <span class="brand-name">recon</span>
        <span class="brand-ver">v0.1.3</span>
      </div>
      <nav class="sidebar-nav">
        <div class="nav-section-label">Fleet</div>
        ${NAV.slice(0,3).map(n => `
          <a href="${n.href}" class="nav-item ${n.key === activeKey ? 'active' : ''}">${I(n.icon)}<span>${n.label}</span>${n.count != null ? `<span class="count">${n.count}</span>` : ''}</a>
        `).join('')}
        <div class="nav-section-label">Investigate</div>
        ${NAV.slice(3,4).map(n => `
          <a href="${n.href}" class="nav-item ${n.key === activeKey ? 'active' : ''}">${I(n.icon)}<span>${n.label}</span>${n.count != null ? `<span class="count">${n.count}</span>` : ''}</a>
        `).join('')}
        <div class="nav-section-label">System</div>
        ${NAV.slice(4).map(n => `
          <a href="${n.href}" class="nav-item ${n.key === activeKey ? 'active' : ''}">${I(n.icon)}<span>${n.label}</span>${n.count != null ? `<span class="count">${n.count}</span>` : ''}</a>
        `).join('')}
      </nav>
      <div class="sidebar-footer">
        <div class="user-row">
          <span class="avatar">vs</span>
          <div style="flex:1;min-width:0">
            <div style="color:var(--fg-0);font-size:12px">vasyansk</div>
            <div style="font-size:10.5px;color:var(--fg-3)">admin</div>
          </div>
          <button class="btn icon ghost" title="Log out" style="color:var(--fg-2)">${I('logout')}</button>
        </div>
        <div style="display:flex;align-items:center;gap:6px;font-family:var(--font-mono);font-size:11px">
          <span class="dot ok pulse"></span>
          <span style="color:var(--fg-1)">hub</span>
          <span class="muted">→ 4/5 online</span>
        </div>
      </div>
    </aside>`;
  }

  // Fixture: k8s_cert_expired scenario
  const FIXTURE = {
    investigation: {
      id: 'inv_01JK3Z5VPQAF',
      goal: 'kubelet на m3 не джойнится в кластер после ребута; остальные ноды живы. Диагностировать причину и определить scope.',
      status: 'active',
      model: 'anthropic/claude-sonnet-4.5',
      created_at: '2026-04-17T14:02:11Z',
      started_by: 'vasyansk',
      steps_used: 7,
      steps_budget: 40,
      tokens_used: 48210,
      tokens_budget: 500000,
    },
    hosts: [
      { id: 'm1.prod.cluster.local', labels: { env:'prod', role:'k8s-master', dc:'msk-1' }, status:'online',  ip:'10.0.0.11', kernel:'5.15.0-88-generic', ram:16, cpu:8, last_seen:'just now' },
      { id: 'm2.prod.cluster.local', labels: { env:'prod', role:'k8s-master', dc:'msk-1' }, status:'online',  ip:'10.0.0.12', kernel:'5.15.0-88-generic', ram:16, cpu:8, last_seen:'just now' },
      { id: 'm3.prod.cluster.local', labels: { env:'prod', role:'k8s-master', dc:'msk-1' }, status:'degraded',ip:'10.0.0.13', kernel:'5.15.0-88-generic', ram:16, cpu:8, last_seen:'12s ago' },
      { id: 'w1.prod.cluster.local', labels: { env:'prod', role:'k8s-worker', dc:'msk-1' }, status:'online',  ip:'10.0.0.21', kernel:'5.15.0-88-generic', ram:32, cpu:16, last_seen:'just now' },
      { id: 'w2.prod.cluster.local', labels: { env:'prod', role:'k8s-worker', dc:'msk-1' }, status:'offline', ip:'10.0.0.22', kernel:'5.15.0-88-generic', ram:32, cpu:16, last_seen:'2h ago' },
    ],
    timeline: [
      { seq:1, tool:'list_hosts', status:'executed', rationale:'Ground myself in the fleet. Narrow to role=k8s-master.', input:{ selector:'role=k8s-master' }, duration:184, task_id:'t_01JK3Z6A0001' },
      { seq:2, tool:'collect', status:'executed', rationale:'Check systemd kubelet state on m3. It is the degraded node.', input:{ host_id:'m3.prod.cluster.local', collector:'systemd_units', params:{ filter:'kubelet.service' } }, duration:312, task_id:'t_01JK3Z6A0002', hint:'service.inactive' },
      { seq:3, tool:'collect', status:'executed', rationale:'Pull last 30 minutes of kubelet journal to see why it fails to start.', input:{ host_id:'m3.prod.cluster.local', collector:'journal_tail', params:{ unit:'kubelet.service', since:'30m' } }, duration:412, task_id:'t_01JK3Z6A0003', hint:'cert.expired' },
      { seq:4, tool:'search_artifact', status:'executed', rationale:'Grep the journal for x509 expiration to confirm the pattern.', input:{ task_id:'t_01JK3Z6A0003', artifact_name:'journalctl.txt', pattern:'x509: certificate has expired' }, duration:47, task_id:'t_01JK3Z6A0004' },
      { seq:5, tool:'add_finding', status:'finding', rationale:'Evidence is solid — kubelet serving cert on m3 is expired.', input:{ severity:'critical', code:'cert.expired.kubelet', message:'kubelet serving cert on m3 expired 2026-04-17 — service cannot start', evidence_refs:['t_01JK3Z6A0003','t_01JK3Z6A0004'] }, duration:3 },
      { seq:6, tool:'collect_batch', status:'executed', rationale:'Check if other masters are close to expiry to pre-empt cluster-wide outage.', input:{ host_ids:['m1.prod.cluster.local','m2.prod.cluster.local'], collector:'file_read', params:{ path:'/var/lib/kubelet/pki/kubelet.crt' } }, duration:521, task_id:'t_01JK3Z6A0006' },
      { seq:7, tool:'add_finding', status:'finding', rationale:'m1 cert expires in 6 days. This is a blast radius issue, not just m3.', input:{ severity:'warn', code:'cert.expiring.kubelet', message:'kubelet cert on m1 expires in 6d — cluster-wide auto-renewal is broken', evidence_refs:['t_01JK3Z6A0006'] }, duration:3 },
    ],
    pending: {
      seq: 8,
      tool: 'collect',
      rationale: 'Need to see if kubelet was configured for auto-renewal. Read /var/lib/kubelet/config.yaml and kube-controller-manager flags on m1 to find why rotation stopped.',
      input: {
        host_id: 'm1.prod.cluster.local',
        collector: 'file_read',
        params: { path: '/var/lib/kubelet/config.yaml' },
        timeout_seconds: 10
      },
      estimated_tokens: 2400,
    },
    findings: [
      { id:'f-1', severity:'critical', code:'cert.expired.kubelet', message:'kubelet serving cert on m3 expired 2026-04-17T03:22Z — service refuses to start', evidence_refs:['t_01JK3Z6A0003','t_01JK3Z6A0004'], pinned:true, ignored:false, created_at:'14:04:22' },
      { id:'f-2', severity:'warn', code:'cert.expiring.kubelet', message:'m1 kubelet cert expires in 6d — blast radius likely cluster-wide', evidence_refs:['t_01JK3Z6A0006'], pinned:false, ignored:false, created_at:'14:05:07' },
      { id:'f-3', severity:'info', code:'service.inactive', message:'kubelet.service on m3 is inactive (dead) since 14:03:58', evidence_refs:['t_01JK3Z6A0002'], pinned:false, ignored:false, created_at:'14:03:19' },
      { id:'f-4', severity:'warn', code:'pod.restart.coredns', message:'CoreDNS pod on m3 restarted 12 times in last hour', evidence_refs:['t_01JK3Z6A0003'], pinned:false, ignored:true, created_at:'14:04:01' },
    ],
    messages: [
      { role:'user', who:'vasyansk', when:'14:02:11', body:'kubelet на m3 не джойнится после ребута. остальные ноды живы. разберись что там.' },
      { role:'assistant', who:'investigator', when:'14:02:24', thinking:'Оператор описал цель. Начну с list_hosts чтобы понять инвентарь — нужно знать, какие хосты мастера, что про m3, и есть ли у m3 статус online/offline в реестре хаба.', body:'tool_use: list_hosts {selector: "role=k8s-master"}' },
      { role:'assistant', who:'investigator', when:'14:03:02', thinking:'m3 помечен degraded — heartbeat агента жив, но что-то не так. Systemd kubelet — первый очевидный кандидат. Узкий запрос, дешевле чем вытягивать всю таблицу юнитов.', body:'tool_use: collect {systemd_units on m3}' },
      { role:'assistant', who:'investigator', when:'14:04:15', thinking:'Hint "cert.expired" от коллектора — сильный сигнал. Фильтрую journal по x509, чтобы не тянуть 800 строк в контекст.', body:'tool_use: search_artifact {x509: certificate has expired}' },
      { role:'system', who:'hub', when:'14:04:31', body:'Finding F-4 "CoreDNS pod restarted 12 times on m3" marked IGNORED by operator. Branch closed.' },
    ],
    live: true,
  };

  // Pretty JSON renderer (syntax-highlighted)
  function highlight(obj, indent = 2) {
    const s = JSON.stringify(obj, null, indent);
    return s
      .replace(/&/g,'&amp;').replace(/</g,'&lt;')
      .replace(/"([^"]+)":/g,'<span class="k">"$1"</span><span class="p">:</span>')
      .replace(/: "([^"]*)"/g,'<span class="p">:</span> <span class="s">"$1"</span>')
      .replace(/: (-?\d+\.?\d*)/g,'<span class="p">:</span> <span class="n">$1</span>')
      .replace(/: (true|false)/g,'<span class="p">:</span> <span class="b">$1</span>')
      .replace(/: null/g,'<span class="p">:</span> <span class="nl">null</span>')
      .replace(/([{}\[\],])/g,'<span class="p">$1</span>');
  }

  // Compact single-line JSON preview
  function preview(obj, max = 80) {
    const s = JSON.stringify(obj);
    return s.length > max ? s.slice(0, max) + '…' : s;
  }

  // Toast helper
  function toast(msg, kind = 'info') {
    let root = document.querySelector('.toasts');
    if (!root) { root = document.createElement('div'); root.className = 'toasts'; document.body.appendChild(root); }
    const el = document.createElement('div');
    el.className = 'toast';
    el.innerHTML = `<span class="dot ${kind}"></span><span>${msg}</span>`;
    root.appendChild(el);
    setTimeout(() => { el.classList.add('fade-out'); setTimeout(() => el.remove(), 200); }, 3200);
  }

  // Tweaks panel render
  function mountTweaks(opts = {}) {
    const { onPendingVariant, onDemo, demoActions = [] } = opts;

    // Defaults from localStorage
    const D = (window.RECON_DEFAULTS || {});
    const aesthetic = localStorage.getItem('recon.aesthetic') || D.aesthetic || 'k9s';
    const density   = localStorage.getItem('recon.density')   || D.density   || 'compact';
    const variant   = localStorage.getItem('recon.pending')   || D.pending   || 'framed';
    document.documentElement.setAttribute('data-aesthetic', aesthetic);
    document.documentElement.setAttribute('data-density', density);
    document.querySelectorAll('.pending').forEach(el => el.setAttribute('data-variant', variant));

    const demoHtml = demoActions.length ? `
      <div class="tweaks-sec">
        <div class="sec-lbl">Demo controls (SSE sim)</div>
        <div class="tweaks-row demo">
          ${demoActions.map(a => `<button data-demo="${a.key}">${a.label}</button>`).join('')}
        </div>
      </div>` : '';

    const el = document.createElement('div');
    el.className = 'tweaks';
    el.innerHTML = `
      <div class="tweaks-hd">
        <span>Tweaks</span>
        <button class="btn icon ghost" id="tweaks-close" title="Close">${I('x')}</button>
      </div>
      <div class="tweaks-body">
        <div class="tweaks-sec">
          <div class="sec-lbl">Aesthetic</div>
          <div class="tweaks-row" data-group="aesthetic">
            <button data-v="linear">Linear</button>
            <button data-v="k9s">k9s</button>
            <button data-v="sentry">Sentry</button>
            <button data-v="grafana">Grafana</button>
          </div>
        </div>
        <div class="tweaks-sec">
          <div class="sec-lbl">Density</div>
          <div class="tweaks-row" data-group="density">
            <button data-v="compact">Compact</button>
            <button data-v="comfy">Comfy</button>
          </div>
        </div>
        <div class="tweaks-sec">
          <div class="sec-lbl">Pending card variant</div>
          <div class="tweaks-row" data-group="pending">
            <button data-v="framed">Framed</button>
            <button data-v="spotlight">Spotlight</button>
            <button data-v="rail">Rail</button>
            <button data-v="inline">Inline</button>
          </div>
        </div>
        ${demoHtml}
      </div>
    `;
    document.body.appendChild(el);

    // Mark active state
    function syncActive() {
      el.querySelectorAll('[data-group="aesthetic"] button').forEach(b => b.classList.toggle('on', b.dataset.v === document.documentElement.getAttribute('data-aesthetic')));
      el.querySelectorAll('[data-group="density"] button').forEach(b => b.classList.toggle('on', b.dataset.v === document.documentElement.getAttribute('data-density')));
      el.querySelectorAll('[data-group="pending"] button').forEach(b => b.classList.toggle('on', b.dataset.v === (document.querySelector('.pending')?.getAttribute('data-variant') || 'framed')));
    }
    syncActive();

    el.querySelector('#tweaks-close').onclick = () => el.style.display = 'none';

    el.querySelectorAll('[data-group="aesthetic"] button').forEach(b => {
      b.onclick = () => {
        document.documentElement.setAttribute('data-aesthetic', b.dataset.v);
        localStorage.setItem('recon.aesthetic', b.dataset.v);
        syncActive();
      };
    });
    el.querySelectorAll('[data-group="density"] button').forEach(b => {
      b.onclick = () => {
        document.documentElement.setAttribute('data-density', b.dataset.v);
        localStorage.setItem('recon.density', b.dataset.v);
        syncActive();
      };
    });
    el.querySelectorAll('[data-group="pending"] button').forEach(b => {
      b.onclick = () => {
        document.querySelectorAll('.pending').forEach(p => p.setAttribute('data-variant', b.dataset.v));
        localStorage.setItem('recon.pending', b.dataset.v);
        if (onPendingVariant) onPendingVariant(b.dataset.v);
        syncActive();
      };
    });
    el.querySelectorAll('[data-demo]').forEach(b => {
      b.onclick = () => onDemo && onDemo(b.dataset.demo);
    });
  }

  return { I, BRAND, NAV, renderSidebar, FIXTURE, highlight, preview, toast, mountTweaks };
})();
