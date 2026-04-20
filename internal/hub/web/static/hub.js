/* Recon Hub — minimal client-side helpers.
   Sidebar + brand mark + active-nav highlight are server-rendered in
   layout.html. This file is reserved for future SSE/timeline interactions
   added in F4 (investigation_detail). */
(function () {
  'use strict';

  // Click-to-copy for any element with a [data-copy] attribute.
  // Copies data-copy to clipboard, flashes "copied" class for 1.2s.
  // Falls back to nothing on browsers without navigator.clipboard
  // (rare; spec-stable since 2018).
  document.addEventListener('click', function (ev) {
    var el = ev.target.closest('[data-copy]');
    if (!el) return;
    var val = el.getAttribute('data-copy');
    if (!val || !navigator.clipboard) return;
    navigator.clipboard.writeText(val).then(function () {
      el.classList.add('copied');
      setTimeout(function () { el.classList.remove('copied'); }, 1200);
    });
  });

  // Update-hint modal: shows the manual one-liner to pull a newer release
  // tarball and restart the agent. Agents with `auto_update: true` in
  // agent.yaml will pick up the new version on their own cadence — this is
  // the "force it now" path.
  window.showUpdateHint = function (hostID, current, latest, arch, releasesURL) {
    arch = arch || 'amd64';
    // releasesURL is like https://github.com/<owner>/<repo>/releases — strip
    // the trailing "/releases" and build the direct tarball URL.
    var base = (releasesURL || 'https://github.com/vasyakrg/reconops').replace(/\/releases\/?$/, '');
    var tarURL = base + '/releases/download/' + latest + '/recon-agent-linux-' + arch + '.tar.gz';
    var cmd = [
      '# On ' + hostID + ' (' + current + ' → ' + latest + '):',
      'curl -sSfL ' + tarURL + ' | tar xz -C /tmp && \\',
      '  install -m0755 /tmp/recon-agent-linux-' + arch + '/bin/recon-agent /usr/local/bin/recon-agent && \\',
      '  systemctl restart recon-agent'
    ].join('\n');
    var overlay = document.createElement('div');
    overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:1000;display:flex;align-items:center;justify-content:center;padding:24px';
    overlay.onclick = function (e) { if (e.target === overlay) overlay.remove(); };
    var card = document.createElement('div');
    card.className = 'card';
    card.style.cssText = 'max-width:780px;width:100%;background:var(--bg-1);border:1px solid var(--border-hi);padding:16px';
    card.innerHTML =
      '<div style="font-size:11px;text-transform:uppercase;letter-spacing:0.06em;color:var(--fg-2);margin-bottom:8px">Update ' + hostID + ' to <span style="color:var(--accent)">' + latest + '</span></div>' +
      '<pre id="upd-cmd" style="user-select:all;white-space:pre-wrap;word-break:break-all;margin:0 0 10px"></pre>' +
      '<div style="display:flex;gap:8px;flex-wrap:wrap;align-items:center">' +
      '  <button type="button" id="upd-copy" class="btn xs">copy</button>' +
      '  <button type="button" id="upd-close" class="btn xs ghost">close</button>' +
      '  <span class="dim" style="font-size:10.5px">Agents with <code>auto_update: true</code> upgrade themselves; this is the manual path.</span>' +
      '</div>';
    overlay.appendChild(card);
    document.body.appendChild(overlay);
    card.querySelector('#upd-cmd').textContent = cmd;
    card.querySelector('#upd-close').onclick = function () { overlay.remove(); };
    card.querySelector('#upd-copy').onclick = function () {
      if (navigator.clipboard) {
        navigator.clipboard.writeText(cmd).then(function () {
          card.querySelector('#upd-copy').textContent = 'copied ✓';
        });
      }
    };
  };

  // Highlight the sidebar nav item that matches the current pathname.
  // Each nav-item carries data-nav="<key>"; layout.html sets data-active on
  // <body> so server can also mark it, but this catches client-side route
  // changes if/when we add them.
  document.addEventListener('DOMContentLoaded', function () {
    var active = document.body.getAttribute('data-active') || '';
    if (!active) return;
    document.querySelectorAll('.nav-item[data-nav]').forEach(function (el) {
      if (el.getAttribute('data-nav') === active) el.classList.add('active');
    });
  });
})();
