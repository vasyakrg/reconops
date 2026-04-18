# Handoff: Recon Hub — LLM-driven diagnostic ops console

## Overview

**Recon** is a read-only fleet diagnostic tool for a small Linux server park (≤20 hosts). The central idea: operators describe a problem in natural language ("kubelet на m3 не джойнится после ребута"), and an LLM-driven *investigator* plans and executes diagnostic steps — running whitelisted read-only collectors on agents over mTLS, reading the resulting artifacts via a retrieval tool, and building up a live, streaming timeline of tool calls and findings. The hub itself holds the database, web UI, and LLM client; agents on each host expose only a fixed catalog of read-only collectors (systemd units, journal tail, network state, proc snapshot, whitelisted file reads, etc). By construction, no write operations exist — the product's core promise is "safe to point at production."

This handoff covers **10 screens** covering investigations, hosts, runs, collectors, audit, settings, and login.

## About the design files

The files in `prototype/` are **design references created in HTML** — static+SSE-simulated prototypes showing intended look and behavior, not production code to copy directly. The implementation task is to **recreate these designs in the target codebase's environment** (the PRD suggests Go backend + any modern SPA framework — React is a safe default) using its established patterns, component library, and state management.

The HTML prototypes lean heavily on inline rendering, hand-written tables, and a single shared CSS file. In the real app, expect to:

- Replace hand-written tables with a real data-table component (TanStack Table, AG Grid, or similar)
- Replace the fake SSE simulation (`investigation_detail.html` lines ~350–430, `fireStep()` / `addFinding()`) with a real `EventSource` subscribed to `/api/investigations/:id/events`
- Replace `localStorage.getItem('recon.aesthetic')` etc. with user-preferences stored server-side
- Replace the hard-coded fixture in `static/hub.js` with live API data

## Fidelity

**High-fidelity.** Colors, typography, spacing, iconography style, timeline structure, badge language, and the pending-tool-call card layout are all intentional and should be reproduced pixel-faithfully. Approved defaults (baked into `investigation_detail.html` via `/*EDITMODE-*/` markers):

- **Aesthetic:** `k9s` — dark with **green** accent (`#a8ff7a`)
- **Density:** `compact` — 28px row heights, `10px 12px` card padding
- **Pending-card variant:** `framed` — the default "in-flight tool call" card style (bordered box with pulsing border)

Other aesthetics (`linear`, `sentry`, `grafana`) and variants (`spotlight`, `rail`, `inline`) are exploratory and can be dropped from production unless the team wants them as user prefs.

## Screens / views

Each screen's HTML is in `prototype/`. Open `prototype/index.html` as the entry point — it links everything.

### 1. Login (`login.html`) — P2
Centered 380px card on a radial-gradient background. Brand mark (SVG compass with crosshair lines, in accent color) + wordmark "recon" above the card. Username + password + primary button. Subtext: `hub.example.com:8080 · v0.1.3` in mono, then a reassurance line: "read-only diagnostic system · by construction no write operations exist".

### 2. Investigations list (`investigations.html`) — P0
Full-width table with columns: ID (mono, truncated), Goal (single-line trunc, 440px max), Status (badge + colored dot), Steps (`7/40`), Tokens (`48.2k`), Findings (mini-bar: 3px vertical stripes per severity + count glyph like `1c 2w 1i`), Created, Updated. Rows clickable → investigation detail. Filter chips in header (`all 22`, `active 3`, `done`, `+ New investigation`).

### 3. **Investigation detail** (`investigation_detail.html`) — P0 — **main screen**
This is the heart of the product. 3-column grid:

- **Col 1 — Timeline** (narrow): vertical list of steps, each a compact row with step index, tool name (mono), status dot (ok/err/pending), elapsed ms, host target, and a one-line summary. Expandable: clicking a row opens the full step card below it (collapsed `search_artifact` queries, tool-call args, tool-return artifact refs). A "pending" row at the bottom is the **live** one — it has a pulsing accent border in `framed` variant, tickings ms counter, and streaming argument text.
- **Col 2 — Findings** (widest): filter bar `FINDINGS · 1c · 1w · 1i` with severity pills. Each finding is a card: severity badge (critical red / warn yellow / info blue), finding code in mono (`cert.expired.kubelet`), timestamp, 1-line message, `evidence: artifact_id …` refs that resolve to tool-return ids, and actions (pin / ignore / fork). A "pinned" finding has a left border accent and a pin icon.
- **Col 3 — Summary & actions** (narrow): running goal text, budgets (steps 7/40 with bar, tokens 48.2k/500k with bar), current status badge, `Fork` / `Export .md` / `Abort` buttons, "latest artifact" preview (collapsed first-line JSON → click to expand).

Header strip above the grid: crumbs `Investigations / inv_01JK3Z5VPQAF`, status badge (`active` with pulsing dot), model chip (`model sonnet-4.5`), live SSE pulse (`live · SSE` with pulsing green dot), then budget progress + action buttons on the right. Goal block sits between header and the 3-col grid.

**Motion & SSE behavior:**
- "live · SSE" pulse pip in header pulses every 1.1s
- On `fireStep()` (demo or real SSE): new row slides in from top of col 1 with a 120ms slide+fade, the pending row below ticks its ms counter every 100ms, `steps-num` and `steps-bar` animate
- On `addFinding()`: new finding card slides in col 2 with a brief yellow → transparent left-border flash (300ms), findings-count in header updates
- Tweaks panel (floating, top-right) exposes aesthetic, density, pending-card variant, plus demo controls: `▶ Fire next step`, `+ Add finding`, `‖ Toggle waiting`, `↻ Reset`

### 4. Hosts list (`hosts.html`) — P0
Table: status dot column (30px), Host ID (mono), Labels (chips in an inline flex-wrap — each chip is `env=prod` with muted `=`), IP, OS+kernel (2-line), CPU, RAM, Agent version, Last seen, `⋯` menu. Header has a label-selector search input (`role=k8s-master,env=prod`) in mono.

### 5. Host detail (`host_detail.html`) — P0
Two-column layout (320px sidebar + flexible main):
- **Sidebar:** Identity card (grid of label/value pairs — host_id, ip, cpu, ram, os, kernel, agent, enrolled, cert fingerprint), Labels card with chips + edit, Heartbeat histogram (60 vertical bars, last 2h, with red bars marking offline gaps).
- **Main:** Tab bar (Overview / Recent runs / Recent findings / Collectors / Audit), Available collectors panel (10 rows each with name in mono + short description + `run` button), Recent findings panel referencing this host.

### 6. Runs list (`runs.html`) — P0
Table: Run ID (mono), Collector (mono, accent color), Hosts (mono, comma list like `m1,m2` or `5 hosts`), Investigation (mono, dim), Status (badge ok/partial/error), Started, Duration.

### 7. Run detail (`run_detail.html`) — P0
Summary panel across the top (grid of 5 fields: Collector, Params, Selector, Started, Duration in monospace), then per-host fan-out table: status dot, host, status badge, per-host duration, result summary (1 line), artifact ref + size, `json` button to open raw artifact.

### 8. Collectors registry (`collectors.html`) — P1
2-column grid of collector cards. Each card: name (mono, 13px), category badge, version chip, description, grid of metadata (reads, requires, available N/M agents), expandable `▸ input schema` (`<details>` element revealing JSON schema).

### 9. Audit log (`audit.html`) — P1
Single list with 4-column rows: timestamp (mono), actor (avatar + name), action (mono, colored by type — green for approved/enrolled, yellow for finding, red for offline, accent for the rest), details (mono, dim). Types to surface: `investigation.created`, `tool_call.proposed`, `tool_call.approved`, `tool_call.executed`, `finding.added`, `finding.ignored`, `run.created`, `agent.enrolled`, `agent.offline`, `bootstrap.token.issued`.

### 10. Settings (`settings.html`) — P1
2-column layout (1fr + 300px): main column has three cards (LLM endpoint, Budgets, Storage) each with a 2-column label/input grid; sidebar has Bootstrap tokens (list with TTL + used state), Admins (avatar rows), mTLS (hub cert expiry + CA name in mono). Footer: `Revert` / `Save changes` actions.

## Global layout — every screen

All non-login screens share the same shell (see `static/hub.js` `renderSidebar()` and `static/hub.css`):

- **Sidebar**, left, 220px, full-height:
  - Brand block at top (32px compass SVG mark + "recon" wordmark + version pill `v0.1.3`)
  - Nav groups with uppercase mono section labels — **FLEET** (Hosts, Collectors, Runs), **INVESTIGATE** (Investigations with count badge), **SYSTEM** (Audit, Settings)
  - User block at bottom (avatar + username + role + logout button)
- **Main area**, fills remaining width:
  - `pg-hd` page header: title + sub-line, or crumbs + badges, with right-side action buttons
  - `pg-body` page body with 18–24px padding

Minimum viewport: **1280px**. The mobile-warn banner at `.mobile-warn` shows when `<1280px`.

## Interactions & behavior

- All list rows clickable → navigate to detail
- Tweaks button (top-right floating `⚙`) toggles the Tweaks panel (draggable/fixed top-right, 280px wide)
- Tweaks panel:
  - Aesthetic picker → sets `data-aesthetic` on `<html>`, persists to `localStorage.recon.aesthetic`
  - Density picker → sets `data-density` on `<html>`, persists
  - Pending-card variant picker → sets `data-variant` on `.pending` elements
  - SSE sim buttons → call `fireStep()` / `addFinding()` / `toggleWaiting()` / `reset()` on investigation detail only
- Pending tool-call card:
  - `data-variant="framed"` (default): bordered box with subtle pulsing border accent
  - `data-variant="spotlight"`: glow/halo behind card
  - `data-variant="rail"`: left accent rail (3px) + transparent body
  - `data-variant="inline"`: no container, just an inline row
- Animations:
  - `.pulse` dot: 1.1s ease infinite scale 1 → 1.35
  - Live SSE pill: same pulse, plus accent-bg → transparent fade
  - Row slide-in: `transform: translateY(-4px)` + `opacity: 0` → settle, 160ms, `--ease-out` (`cubic-bezier(0.2, 0.9, 0.3, 1)`)
  - Finding card add: left-border flash `var(--warn)` → `var(--border-accent)` over 300ms
  - Pending ms counter: ticks every 100ms while status is `pending`

## State management (in real impl)

- Global: current user, theme preferences (aesthetic/density), active investigation id if any
- Per investigation: `{id, goal, model, status, steps[], findings[], budgets: {steps_used, steps_max, tokens_used, tokens_max}, created_at, updated_at}`
- SSE event stream shape — subscribe once investigation is loaded:
  - `step.started {id, tool, args, host_target}`
  - `step.completed {id, artifact_id, duration_ms, status}`
  - `finding.added {id, severity, code, message, evidence_artifact_ids[]}`
  - `finding.updated {id, pinned?, ignored?}`
  - `budget.update {steps_used, tokens_used}`
  - `status.changed {status}`
- `search_artifact` requests: fire-and-forget HTTP to the hub, collapsed previews in UI

## Design tokens — exact values

Full CSS variables in `prototype/static/hub.css`. Key tokens:

```
/* Dark base */
--bg-0: #0d0f12   /* app bg */
--bg-1: #13161b   /* panels */
--bg-2: #181c22   /* header / sunken */
--bg-3: #1d2026
--bg-4: #24272f
--border:       #2a2e36
--border-hi:    #3a3f4a
--border-accent:#3a4a73  (k9s: #2f4a28)

--fg-0: #e6e8eb   /* primary */
--fg-1: #b4b9c2
--fg-2: #8a8f9a
--fg-3: #606670   /* dim / mono annotations */

/* Accent — k9s default */
--accent:     #a8ff7a
--accent-hi:  #c2ff9f
--accent-dim: #2f4a28
--accent-bg:     rgba(168,255,122,0.07)
--accent-bg-hi:  rgba(168,255,122,0.14)

/* Semantic */
--ok:       #4ec9a4
--warn:     #e5b567
--err:      #e06c75
--info:     #7aa2ff
--pending:  #c3a6e0
--critical: #ff6d8a
/* each has a matching <name>-bg at 10% alpha */

/* Type */
--font-sans: 'Inter', 'Geist Sans', system-ui, sans-serif
--font-mono: 'JetBrains Mono', 'Geist Mono', ui-monospace, monospace

/* Radii */
--r-sm: 3px   (chips, small buttons)
--r-md: 4px   (inputs, standard buttons)
--r-lg: 6px   (panels, cards)

/* Motion */
--ease-out: cubic-bezier(0.2, 0.9, 0.3, 1)
--ease-in:  cubic-bezier(0.5, 0, 0.85, 0.3)

/* Density (compact = default) */
--row-h:    28px   (compact) / 34px (comfy)
--pad-card: 10px 12px (compact) / 14px 16px (comfy)
```

**Type scale** (used in prototypes):
- Display / page title: 18–22px, Inter 600
- Section title: 13–14px, Inter 500, color `--fg-0`
- Body: 12.5–13px, Inter 400, color `--fg-1`
- Mono: 11–12px, JetBrains Mono 400, color context-dependent
- Micro / labels: 10–10.5px uppercase, letter-spacing 0.06–0.08em, color `--fg-3`

**Spacing rhythm**: mostly 4/6/8/10/12/14/16/18/24 px. No large decorative gaps — the aesthetic is dense and utilitarian.

## Assets

- **Brand mark**: a small compass-like SVG icon (dashed outer circle + crosshair + a radar-needle line) — defined as `BRAND` in `prototype/static/hub.js`. Used on login, sidebar, and index.
- **Iconography**: lucide-style (stroke 1.4, round caps) for inline affordances — we only used a handful (refresh, pin, fork, chevrons, dots). Use `lucide-react` in a real React impl.
- **No photography, no illustration.** Everything is CSS + SVG.
- **Fonts**: Inter and JetBrains Mono loaded from Google Fonts in each HTML file.

## Files

In this handoff:
- `prototype/index.html` — design index with links to all screens
- `prototype/login.html` — P2 Login
- `prototype/investigations.html` — P0 Investigations list
- `prototype/investigation_detail.html` — P0 **Investigation detail (main screen)**
- `prototype/hosts.html` — P0 Hosts list
- `prototype/host_detail.html` — P0 Host detail
- `prototype/runs.html` — P0 Runs list
- `prototype/run_detail.html` — P0 Run detail
- `prototype/collectors.html` — P1 Collectors registry
- `prototype/audit.html` — P1 Audit log
- `prototype/settings.html` — P1 Settings
- `prototype/static/hub.css` — single source of truth for design tokens, layout, components
- `prototype/static/hub.js` — shared brand mark, sidebar render, fixture data, Tweaks panel, SSE simulation helpers

## Open questions for the dev

- Real `EventSource` URL shape — `/api/investigations/:id/events` or Server-Sent events multiplexed from a single endpoint?
- User preferences (aesthetic/density) — stored server-side per user, or client-only?
- Ship with just the k9s aesthetic and drop the other three? The other aesthetics are behind Tweaks but not requested as real product features.
- Real artifact size limits + pagination for `search_artifact` previews in the timeline.
