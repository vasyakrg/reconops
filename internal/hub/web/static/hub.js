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
