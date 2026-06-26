/* Recon Hub — minimal client-side helpers.
   Sidebar + brand mark + active-nav highlight are server-rendered in
   layout.html. This file is reserved for future SSE/timeline interactions
   added in F4 (investigation_detail). */
(function () {
  'use strict';

  // Operator timezone. The server renders timestamps in the zone named by the
  // `tz` cookie (default UTC). Detect the browser's IANA zone via Intl and, when
  // it differs from the cookie, persist it and reload ONCE so the server-side
  // render localizes. Guarded against a reload loop: we only reload when the
  // cookie value actually changes, so steady state never reloads. All
  // machine-facing timestamps (export, JSON API, SSE, LLM/notebook) stay UTC.
  try {
    var tz = (window.Intl && Intl.DateTimeFormat) ? Intl.DateTimeFormat().resolvedOptions().timeZone : '';
    if (tz) {
      var m = document.cookie.match(/(?:^|;\s*)tz=([^;]+)/);
      var current = m ? decodeURIComponent(m[1]) : '';
      if (current !== tz) {
        document.cookie = 'tz=' + encodeURIComponent(tz) + '; path=/; max-age=31536000; samesite=Lax';
        location.reload();
        return;
      }
    }
  } catch (e) { /* Intl unavailable or blocked → stay on UTC */ }

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

  // No-reload operator approve (Task 5). Forms marked [data-live-preserve]
  // (approve/edit/skip/end, hypothesis, retry, continue) submit via fetch and
  // swap the returned live fragments in place instead of doing a full 303 page
  // reload — the approve→advance loop stays on the page. Progressive
  // enhancement: any failure falls back to a native submit, so the action is
  // never silently lost. data-live-preserve was previously a dead attribute.
  document.addEventListener('submit', function (ev) {
    var form = ev.target;
    if (!form || !form.matches || !form.matches('form[data-live-preserve]')) return;
    if (form.getAttribute('data-live-bypass') === '1') return; // native fallback in progress
    // Only intercept when the live engine is actually running (active / waiting
    // / paused pages). On a terminal page (aborted) the engine never starts, so
    // the retry/continue forms must submit natively — a full 303 reload then
    // boots the engine fresh for the resumed investigation. Without this guard
    // we would fetch-POST (which succeeds), find no engine to swap into, and
    // fall back to a SECOND native POST that the server rejects.
    if (!window.__reconLive) return;
    ev.preventDefault();

    var dbg = function (msg, extra) {
      if (window.console && window.console.debug) {
        window.console.debug('[FIX:investigation-live] ' + msg, extra || {});
      }
    };
    var action = form.getAttribute('action') || '';
    var fd = new FormData(form);
    // A 'submit' event does not add the clicked button to FormData; the decide
    // form carries the chosen action on the button (name="decision"), so add it.
    if (ev.submitter && ev.submitter.name) fd.append(ev.submitter.name, ev.submitter.value);

    var nativeSubmit = function () {
      form.setAttribute('data-live-bypass', '1');
      form.submit(); // programmatic submit does NOT re-dispatch 'submit' → no interception loop
    };

    // Bound the action POST too: a stalled approve must fall back to a native
    // submit (303) rather than hang silently with the operator staring at an
    // unchanged page.
    var ctl = ('AbortController' in window) ? new AbortController() : null;
    var killer = setTimeout(function () {
      if (ctl) { try { ctl.abort(); } catch (e) { /* ignore */ } }
    }, 15000);
    fetch(action, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'X-Requested-With': 'fetch' }, // server returns live fragments instead of a 303
      body: new URLSearchParams(fd),            // urlencoded → server ParseForm reads it directly
      signal: ctl ? ctl.signal : undefined
    }).then(function (resp) {
      if (!resp.ok) throw new Error('action status ' + resp.status);
      if (resp.status === 204) return null;
      return resp.text();
    }).then(function (html) {
      var live = window.__reconLive;
      if (html && live && live.apply(html)) {
        dbg('no-reload action applied', { action: action });
        return;
      }
      // 204 / non-fragment body but the engine is running → reconcile via the
      // change-gated refresh. No running engine (terminal page) → let the
      // server 303 us through a native submit (never a JS page reload).
      if (live) { live.refresh(); return; }
      nativeSubmit();
    }).catch(function (err) {
      dbg('no-reload action failed, falling back to native submit', { action: action, error: String(err) });
      nativeSubmit();
    }).finally(function () {
      clearTimeout(killer);
    });
  }, false);

  // Update-hint modal: shows the manual one-liner to pull a newer release
  // tarball and restart the agent. Agents with `auto_update: true` in
  // agent.yaml will pick up the new version on their own cadence — this is
  // the "force it now" path.
  window.showUpdateHint = function (hostID, current, latest, arch, downloadBase) {
    arch = arch || 'amd64';
    // downloadBase is the release origin root (no trailing "/releases"): the hub's
    // own public base in self-hosted mode, or the GitHub repo root otherwise. The
    // hub serves /releases/download/<tag>/... for the bundled version, mirroring
    // GitHub's asset path, so the same URL shape works in both modes.
    var base = (downloadBase || 'https://github.com/vasyakrg/reconops').replace(/\/+$/, '');
    var tarURL = base + '/releases/download/' + latest + '/recon-agent-linux-' + arch + '.tar.gz';
    // Keep the "# On host (…)" line as on-screen context only — the copy
    // button must copy the runnable command alone, never the comment.
    var header = '# On ' + hostID + ' (' + current + ' → ' + latest + '):';
    var command = [
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
    card.querySelector('#upd-cmd').textContent = header + '\n' + command;
    card.querySelector('#upd-close').onclick = function () { overlay.remove(); };
    card.querySelector('#upd-copy').onclick = function () {
      if (navigator.clipboard) {
        navigator.clipboard.writeText(command).then(function () {
          card.querySelector('#upd-copy').textContent = 'copied ✓';
        });
      }
    };
  };

  // "Direct the model" hypothesis modal. The trigger ([data-hyp-open]) lives
  // inside the live side fragment and is re-rendered on every swap; the modal
  // is built here on document.body so a fragment swap can't destroy an open
  // dialog. CSRF + investigation id are read from the trigger's data-* AT CLICK
  // TIME (never a stale token). The modal owns its fetch — it deliberately does
  // NOT use data-live-preserve — and reconciles via the live engine's refresh().
  (function () {
    var FOCUSABLE = 'a[href],button:not([disabled]),textarea:not([disabled]),input:not([disabled]),[tabindex]:not([tabindex="-1"])';
    var overlay = null, previousFocus = null, keyHandler = null;
    var dbg = function (msg, extra) {
      if (window.console && window.console.debug) {
        window.console.debug('[FIX:investigation-live] ' + msg, extra || {});
      }
    };
    var closeModal = function () {
      if (!overlay) return;
      document.removeEventListener('keydown', keyHandler, true);
      overlay.remove(); overlay = null; keyHandler = null;
      if (previousFocus && previousFocus.focus) { try { previousFocus.focus(); } catch (e) { /* ignore */ } }
    };
    var openModal = function (invID, csrf) {
      if (overlay) return;
      previousFocus = document.activeElement;
      overlay = document.createElement('div');
      overlay.className = 'hyp-overlay';
      overlay.setAttribute('role', 'presentation');
      // Static markup only — no dynamic data is interpolated into innerHTML
      // (csrf/invID travel in the fetch body, never the DOM string).
      overlay.innerHTML =
        '<div class="hyp-modal" role="dialog" aria-modal="true" aria-label="Direct the model — inject a hypothesis">' +
          '<div class="hyp-modal-hd">' +
            '<div><div class="hd-title">⚠ Direct the model</div>' +
            '<div class="hd-sub">Replaces the current pending step. The model must verify the claim before continuing any branch.</div></div>' +
            '<button type="button" class="close-btn" data-hyp-close aria-label="Close">✕</button>' +
          '</div>' +
          '<div class="hyp-modal-body">' +
            '<div class="hyp-what-happens"><span class="arr">↳</span><span>The current pending step is discarded and replaced with this claim. The model runs to gather evidence, then continues.</span></div>' +
            '<div class="hyp-error-banner" data-hyp-error role="alert"><span>⚠</span><span data-hyp-error-msg></span></div>' +
            '<div class="hyp-field">' +
              '<label for="hyp-claim">Claim <span class="req">required</span></label>' +
              '<textarea id="hyp-claim" rows="4" placeholder="kube-controller-manager stopped auto-renewing certs because the kubeadm maintenance job was skipped"></textarea>' +
              '<div class="field-hint">State the condition you believe is true. The model must verify it before any other branch.</div>' +
            '</div>' +
            '<div class="hyp-field">' +
              '<label for="hyp-expected">Expected evidence <span class="opt">optional</span></label>' +
              '<textarea id="hyp-expected" rows="2" placeholder="openssl x509 -enddate -noout -in /etc/kubernetes/pki/apiserver.crt"></textarea>' +
              '<div class="field-hint">A concrete artifact or command output that would confirm it.</div>' +
            '</div>' +
            '<div class="hyp-field">' +
              '<label for="hyp-instruction">Instruction <span class="opt">optional</span></label>' +
              '<textarea id="hyp-instruction" rows="2" placeholder="verify before resuming other branches"></textarea>' +
              '<div class="field-hint">A constraint on execution order or scope.</div>' +
            '</div>' +
          '</div>' +
          '<div class="hyp-modal-ft">' +
            '<span class="kbd-hint">⌘↵ to inject · Esc to cancel</span>' +
            '<span class="spacer"></span>' +
            '<button type="button" class="btn ghost sm" data-hyp-close>Cancel</button>' +
            '<button type="button" class="btn inject sm" data-hyp-submit disabled>⚡ Inject hypothesis</button>' +
          '</div>' +
        '</div>';
      document.body.appendChild(overlay);

      var modal = overlay.querySelector('.hyp-modal');
      var claim = overlay.querySelector('#hyp-claim');
      var expected = overlay.querySelector('#hyp-expected');
      var instruction = overlay.querySelector('#hyp-instruction');
      var submit = overlay.querySelector('[data-hyp-submit]');
      var errBox = overlay.querySelector('[data-hyp-error]');
      var errMsg = overlay.querySelector('[data-hyp-error-msg]');

      claim.addEventListener('input', function () { submit.disabled = claim.value.trim() === ''; });

      var setBusy = function (busy) {
        if (busy) { submit.classList.add('loading'); } else { submit.classList.remove('loading'); }
        overlay.querySelectorAll('textarea,button').forEach(function (el) { el.disabled = busy; });
        if (!busy) submit.disabled = claim.value.trim() === '';
      };

      var doSubmit = function () {
        if (claim.value.trim() === '') { claim.focus(); return; }
        errBox.classList.remove('show');
        setBusy(true);
        var ctl = ('AbortController' in window) ? new AbortController() : null;
        var killer = setTimeout(function () { if (ctl) { try { ctl.abort(); } catch (e) { /* ignore */ } } }, 15000);
        var body = new URLSearchParams();
        body.set('csrf', csrf);
        body.set('investigation_id', invID);
        body.set('claim', claim.value);
        body.set('expected', expected.value);
        body.set('instruction', instruction.value);
        fetch('/investigations/hypothesis', {
          method: 'POST', credentials: 'same-origin',
          headers: { 'X-Requested-With': 'fetch' },
          body: body, signal: ctl ? ctl.signal : undefined
        }).then(function (resp) {
          if (!resp.ok) throw new Error('status ' + resp.status);
          dbg('hypothesis injected', { investigation_id: invID });
          // The live engine re-fetches every region (the pending step is now
          // replaced). Close after, restoring focus. No full reload.
          if (window.__reconLive && window.__reconLive.id === invID) window.__reconLive.refresh();
          closeModal();
        }).catch(function (err) {
          // Keep the operator's text; surface the failure in-place, stay open.
          setBusy(false);
          var aborted = ctl && ctl.signal && ctl.signal.aborted;
          errMsg.textContent = aborted
            ? 'Network timeout — your text is preserved. Try again or reload.'
            : 'Could not inject (' + String(err).replace(/^Error:\s*/, '') + '). The step was not replaced; your text is preserved.';
          errBox.classList.add('show');
          claim.focus();
        }).finally(function () { clearTimeout(killer); });
      };

      submit.addEventListener('click', doSubmit);
      overlay.addEventListener('click', function (e) {
        if (e.target === overlay || (e.target.closest && e.target.closest('[data-hyp-close]'))) closeModal();
      });
      keyHandler = function (e) {
        if (e.key === 'Escape') { e.preventDefault(); closeModal(); return; }
        if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') { e.preventDefault(); if (!submit.disabled) doSubmit(); return; }
        if (e.key === 'Tab') { // focus trap
          var f = Array.prototype.slice.call(modal.querySelectorAll(FOCUSABLE)).filter(function (el) { return el.offsetParent !== null; });
          if (!f.length) return;
          var first = f[0], last = f[f.length - 1];
          if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
          else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
        }
      };
      document.addEventListener('keydown', keyHandler, true);
      claim.focus();
      dbg('hypothesis modal opened', { investigation_id: invID });
    };

    document.addEventListener('click', function (ev) {
      var trigger = ev.target.closest && ev.target.closest('[data-hyp-open]');
      if (!trigger) return;
      ev.preventDefault();
      openModal(trigger.getAttribute('data-inv-id') || '', trigger.getAttribute('data-csrf') || '');
    });
  })();

  // TERMINAL statuses end the live engine. Everything else (active, waiting,
  // paused) keeps updating — the lifecycle is driven by STATUS, never by the
  // presence of the #live-pulse badge (which only renders for "active" and so
  // used to silently kill updates on ask_operator / budget-paused states).
  var LIVE_TERMINAL = { done: true, aborted: true };

  // ReconInvestigationLive wires the investigation detail page to live server
  // state. Two drivers run together by design:
  //   1. SSE (/investigations/events/{id}) — low-latency push, the fast path.
  //   2. An ALWAYS-ON, change-aware backstop poll (~4.5s) — the reliability
  //      floor. It runs even while SSE is healthy.
  // Both funnel through a single coalesced, fingerprint-gated refresh(), so a
  // missed/dropped event or a silently-stalled SSE stream (proxy buffering,
  // an event that arrived mid-fetch) can no longer leave the page stuck on
  // "Waiting for the model." Change-gating means the redundant poll fetch
  // costs one cheap request every few seconds and never reflows the page —
  // the deliberate trade for "stuck is structurally impossible".
  window.ReconInvestigationLive = function (opts) {
    if (!opts || !opts.id) return;
    var dbg = function (msg, extra) {
      if (window.console && window.console.debug) {
        window.console.debug('[FIX:investigation-live] ' + msg, extra || {});
      }
    };
    // The rendered live region carries the server fingerprint and is the
    // source of truth for change detection — it uses the SAME encoding as the
    // fetched fragments (getAttribute) and the SSE payload. opts.snapshot
    // (printf %q) is encoded differently, so it is only a fallback.
    var initStatus = document.getElementById('investigation-status-fragment');
    var currentSnapshot = (initStatus && initStatus.getAttribute('data-snapshot')) || opts.snapshot || '';

    var es = null;
    var reconnectTimer = null;
    var reconnectDelay = 1000;   // SSE reconnect backoff base (ms), capped below
    var pollTimer = null;
    var watchdogTimer = null;    // independent timer that force-releases a stuck refresh
    var refreshing = false;      // a fragment fetch is in flight
    var pendingRefresh = false;  // a refresh was requested mid-fetch — never drop the latest
    var refreshStartedAt = 0;    // ms timestamp the in-flight fetch began (watchdog input)
    var refreshController = null;// AbortController for the in-flight fetch (when supported)
    var closed = false;          // engine permanently stopped (terminal status / unload)
    var dirty = {};              // region ids whose swap was skipped (focus) and must be retried
    var BACKSTOP_MS = 4500;
    var REFRESH_TIMEOUT_MS = 12000; // abort a stalled fragment fetch so `refreshing` can never stick

    // liveLabel sets the live-pulse badge label, but ONLY when it actually
    // changes. The badge is `display:inline-flex` with no reserved width, so
    // rewriting the text to a different length ("live" ↔ "updating") shifts
    // every sibling in the header flex row — a visible twitch. The previous
    // pulse() rewrote the label on EVERY backstop poll (~4.5s), so the header
    // jittered forever while the page was open, even with the LLM idle and the
    // operator just sitting on a pending-approval prompt (status stays
    // "active", so the badge is rendered). Guarding the write makes the
    // steady state a no-op: the label stays "live" with zero DOM churn, and
    // only a genuine state transition (offline / reconnecting / recovered)
    // ever moves it. [FIX:investigation-live]
    var liveLabel = function (label) {
      var lp = document.getElementById('live-pulse');
      if (!lp || !label) return; // badge only renders for "active" — its absence must NOT stop updates
      var text = lp.querySelector('[data-live-label]');
      if (text && text.textContent !== label) {
        text.textContent = label;
        dbg('live label changed', { investigation_id: opts.id, label: label });
      }
    };
    // heartbeat is a layout-NEUTRAL "we polled" blip: it flashes the .tick
    // class (no width/label change) so a routine poll never reflows the header.
    // The continuously CSS-animated dot is the primary liveness cue.
    var heartbeat = function () {
      var lp = document.getElementById('live-pulse');
      if (!lp) return;
      lp.classList.add('tick');
      setTimeout(function () { lp.classList.remove('tick'); }, 400);
    };
    var activeInside = function (el) {
      return el && document.activeElement && el.contains(document.activeElement);
    };
    // replaceRegion swaps one live region. Returns true when the swap actually
    // applied (or there was nothing to apply), false when it was SKIPPED to
    // preserve operator focus/typing. The caller uses that to decide whether to
    // advance the fingerprint — advancing past a skipped region freezes it.
    var replaceRegion = function (id, doc) {
      var current = document.getElementById(id);
      var next = doc.getElementById(id);
      if (!current || !next) return true;
      // Don't yank a region out from under an operator who is typing in it.
      // Exception: the timeline may swap when the pending step changed (the
      // form they were looking at no longer applies).
      if (activeInside(current)) {
        if (id !== 'investigation-timeline-fragment') return false;
        var samePending = current.getAttribute('data-pending-id') === next.getAttribute('data-pending-id');
        if (samePending) return false;
      }
      current.replaceWith(next);
      return true;
    };
    var statusOf = function (doc) {
      var el = (doc || document).getElementById('investigation-status-fragment');
      return el ? (el.getAttribute('data-status') || '') : '';
    };
    var stop = function () {
      if (closed) return;
      closed = true;
      if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
      if (watchdogTimer) { clearInterval(watchdogTimer); watchdogTimer = null; }
      if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
      if (refreshController) { try { refreshController.abort(); } catch (e) { /* ignore */ } refreshController = null; }
      if (es) { try { es.close(); } catch (e) { /* ignore */ } es = null; }
      if (window.__reconLive && window.__reconLive.id === opts.id) window.__reconLive = null;
      dbg('engine stopped', { investigation_id: opts.id });
    };

    // budgetSig returns a status-region signature with the two budget readout
    // blocks emptied, so two status fragments that differ ONLY in the
    // step/token numbers compare equal — the signal patchStatusInPlace uses to
    // decide a smooth in-place update is safe.
    var budgetSig = function (el) {
      var c = el.cloneNode(true);
      var b = c.querySelectorAll('.budget');
      for (var i = 0; i < b.length; i++) { b[i].innerHTML = ''; }
      return c.innerHTML;
    };
    // patchStatusInPlace updates the status header's budget readouts (steps /
    // tokens text + bar) in place when that is the ONLY difference, returning
    // true on success. The status header churns every turn on those counters but
    // carries no scroll / <details> / typed input, so a full replaceWith would
    // merely repaint (flash) the whole header for a number tick. When anything
    // structural changed (badge, auto-approve, model, buttons) it returns false
    // and the caller does a full swap — harmless, since there is no state to
    // lose. The wrapper's fingerprint attrs are kept in sync so the next diff is
    // accurate.
    var patchStatusInPlace = function (cur, next) {
      if (budgetSig(cur) !== budgetSig(next)) return false;
      var curB = cur.querySelectorAll('.budget');
      var nxtB = next.querySelectorAll('.budget');
      if (curB.length === 0 || curB.length !== nxtB.length) return false;
      for (var i = 0; i < curB.length; i++) { curB[i].innerHTML = nxtB[i].innerHTML; }
      cur.setAttribute('data-frag-hash', next.getAttribute('data-frag-hash') || '');
      cur.setAttribute('data-snapshot', next.getAttribute('data-snapshot') || cur.getAttribute('data-snapshot') || '');
      cur.setAttribute('data-status', next.getAttribute('data-status') || cur.getAttribute('data-status') || '');
      return true;
    };

    // applyFragments reconciles the three live regions with the server, then
    // drives the status lifecycle. Shared by the poll/SSE refresh and the
    // no-reload approve path (Task 5).
    //
    // PER-REGION gate: each region carries its own content fingerprint
    // (data-frag-hash, server-computed from that region's own content). A region
    // is swapped only when ITS hash changed, OR when it was previously SKIPPED to
    // preserve operator focus (`dirty`). This replaces the old single global
    // data-snapshot gate, which embedded updated_at + token counters that churn
    // every turn and therefore reflowed ALL THREE regions on every poll tick —
    // collapsing <details> open state and scroll in the timeline (the reported
    // flicker, worst on a short list). The token/budget churn now lives only in
    // the status hash, so the timeline and side regions stay put across it.
    //
    // The dirty[] focus-retry MUST bypass the hash gate (skip only when clean AND
    // unchanged): a region deferred for operator focus whose hash later stops
    // changing would otherwise freeze permanently (the 918f933 wedge class).
    var REGION_IDS = ['investigation-status-fragment', 'investigation-timeline-fragment', 'investigation-side-fragment'];
    var applyFragments = function (html) {
      var doc = new DOMParser().parseFromString(html, 'text/html');
      var fetchedStatus = doc.getElementById('investigation-status-fragment');
      if (!fetchedStatus) return false; // not a live-fragment response (e.g. full page / error)
      var nextSnapshot = fetchedStatus.getAttribute('data-snapshot') || '';
      REGION_IDS.forEach(function (id) {
        var cur = document.getElementById(id);
        var nxt = doc.getElementById(id);
        var regionChanged = !cur || !nxt ||
          (cur.getAttribute('data-frag-hash') !== nxt.getAttribute('data-frag-hash'));
        if (!regionChanged && !dirty[id]) return; // this region unchanged + clean → leave it (no reflow)
        // Status header: patch the budget readouts in place (no flash) when
        // that's the only change and we're not retrying a focus-skip.
        if (id === 'investigation-status-fragment' && cur && nxt && !dirty[id] &&
            patchStatusInPlace(cur, nxt)) {
          return;
        }
        if (replaceRegion(id, doc)) { delete dirty[id]; } else { dirty[id] = true; }
      });
      if (nextSnapshot) currentSnapshot = nextSnapshot;
      // Idempotent: a no-op in steady state (label already "live"), so a
      // routine poll causes ZERO header reflow; it only restores "live" after
      // a transient offline/reconnecting state. heartbeat() is layout-neutral.
      liveLabel('live');
      heartbeat();
      // Stop only once the terminal state is actually rendered (no region still
      // awaiting a swap) — never strand the page on a stale non-terminal view.
      if (LIVE_TERMINAL[statusOf(doc)] && !dirty['investigation-status-fragment'] &&
          !dirty['investigation-timeline-fragment'] && !dirty['investigation-side-fragment']) {
        dbg('terminal status reached', { investigation_id: opts.id, status: statusOf(doc) });
        stop();
      }
      return true;
    };

    // refresh fetches the live fragments. Coalesced: a request that lands while
    // a fetch is in flight sets pendingRefresh so the LATEST state is fetched
    // afterwards instead of being dropped — the primary stuck-page root cause.
    var refresh = function () {
      if (closed) return;
      if (refreshing) { pendingRefresh = true; return; }
      refreshing = true;
      refreshStartedAt = Date.now();
      // Do NOT relabel to "updating" here: an always-on backstop poll fires
      // every BACKSTOP_MS, and a wider label reflows the header on every tick.
      // A layout-neutral heartbeat conveys the poll without moving anything.
      heartbeat();
      var ctl = ('AbortController' in window) ? new AbortController() : null;
      refreshController = ctl;
      var killer = setTimeout(function () {
        // A stalled fragment fetch must NEVER permanently wedge the engine: a
        // never-settling fetch would leave `refreshing` true forever, and EVERY
        // driver (SSE state-change + backstop poll + post-approve) early-returns
        // on that flag — so one stuck request silently disables all of them.
        // Aborting forces .catch/.finally to run and releases the flag.
        if (ctl) { try { ctl.abort(); } catch (e) { /* ignore */ } }
      }, REFRESH_TIMEOUT_MS);
      fetch('/investigations/fragments/' + encodeURIComponent(opts.id), {
        credentials: 'same-origin',
        headers: { 'X-Requested-With': 'fetch' },
        signal: ctl ? ctl.signal : undefined
      }).then(function (resp) {
        if (!resp.ok) throw new Error('fragment status ' + resp.status);
        return resp.text();
      }).then(function (html) {
        applyFragments(html);
      }).catch(function (err) {
        dbg('refresh failed', { investigation_id: opts.id, error: String(err) });
        liveLabel('offline');
      }).finally(function () {
        clearTimeout(killer);
        refreshController = null;
        refreshing = false;
        if (pendingRefresh && !closed) {
          pendingRefresh = false;
          setTimeout(refresh, 0); // run the coalesced follow-up for the latest state
        }
      });
    };

    // Backstop poll — ALWAYS on, change-aware. The reliability floor that
    // survives a silently-stalled stream. Never stopped on SSE onopen (the old
    // behavior left a healthy-but-quiet stream with no backstop).
    var startPolling = function () {
      if (closed || pollTimer) return;
      pollTimer = setInterval(refresh, BACKSTOP_MS);
    };
    // Watchdog — a SEPARATE timer, because a stuck refresh() cannot rescue
    // itself (it never re-enters past the `refreshing` guard). If a fetch has
    // been in flight past the abort deadline + slack, force-release the flag,
    // abort the request, and re-arm. With the AbortController above this is
    // belt-and-suspenders, but it makes a permanently-stuck `refreshing`
    // structurally impossible — regardless of fetch/Promise quirks.
    var startWatchdog = function () {
      if (closed || watchdogTimer) return;
      watchdogTimer = setInterval(function () {
        if (closed || !refreshing) return;
        if (Date.now() - refreshStartedAt > REFRESH_TIMEOUT_MS + 3000) {
          dbg('watchdog: force-releasing stuck refresh', { investigation_id: opts.id });
          if (refreshController) { try { refreshController.abort(); } catch (e) { /* ignore */ } refreshController = null; }
          refreshing = false;
          pendingRefresh = false;
          refresh();
        }
      }, 5000);
    };

    var scheduleReconnect = function () {
      if (closed || reconnectTimer) return;
      liveLabel('reconnecting');
      reconnectTimer = setTimeout(function () {
        reconnectTimer = null;
        connect();
      }, reconnectDelay);
      reconnectDelay = Math.min(reconnectDelay * 2, 15000); // capped exponential backoff
    };

    var connect = function () {
      if (closed || !('EventSource' in window)) return;
      dbg('connecting event stream', { investigation_id: opts.id });
      es = new EventSource('/investigations/events/' + encodeURIComponent(opts.id));
      es.onopen = function () {
        reconnectDelay = 1000; // healthy connection resets the backoff
        liveLabel('live');
      };
      es.addEventListener('state-change', function (ev) {
        // Server emits state-change only on a fingerprint change; still guard
        // against a duplicate so we don't fetch needlessly.
        if (!ev.data || ev.data !== currentSnapshot) refresh();
      });
      es.addEventListener('bye', function () {
        // Planned server-side timeout is reconnectable, not terminal
        // (regression: 2026-06-11-11.58).
        dbg('server closed stream (bye), reconnecting', { investigation_id: opts.id });
        if (es) { try { es.close(); } catch (e) { /* ignore */ } es = null; }
        scheduleReconnect();
      });
      es.onerror = function () {
        // Transient drop: close and reconnect with backoff. The always-on
        // backstop poll keeps the page fresh while the stream is down.
        if (es) { try { es.close(); } catch (e) { /* ignore */ } es = null; }
        scheduleReconnect();
      };
    };

    window.addEventListener('beforeunload', stop);

    // Expose a small controller so the no-reload approve path (Task 5) can
    // reuse the same change-gated swap + lifecycle instead of a full reload.
    var controller = { id: opts.id, refresh: refresh, apply: applyFragments, stop: stop };
    window.__reconLive = controller;

    // If we somehow started on an already-terminal page, do nothing further.
    if (LIVE_TERMINAL[statusOf(null)]) { stop(); return controller; }

    startPolling();          // reliability floor first
    startWatchdog();         // make a stuck refresh structurally impossible
    if ('EventSource' in window) connect(); // then the low-latency fast path
    return controller;
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
