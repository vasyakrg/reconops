/* Recon Hub — minimal client-side helpers.
   Sidebar + brand mark + active-nav highlight are server-rendered in
   layout.html. This file is reserved for future SSE/timeline interactions
   added in F4 (investigation_detail). */
(function () {
  'use strict';

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
