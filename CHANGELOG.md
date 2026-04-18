# Changelog

All notable changes to **Recon** are documented here. The project followed a
five-week MVP plan; each week shipped as one or two feature commits plus a
follow-on security-review fix commit.

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions: not yet tagged — pre-1.0.

## [Unreleased] — Post-MVP

The MVP closed at commit `ebc0c5f`. Everything below ships incrementally
on top — no new MVP scope, just the productionisation track: k9s
redesign, docker compose, GitHub Actions release pipeline, quick install,
scoped investigations, session persistence, network/TLS hardening.

### Web — k9s redesign (F1–F4) (`d65c60a` `289912b` `e111ce9` `ecf3916`)

**Added**
- F1 shell: 220px sidebar (Fleet / Investigate / System), brand mark,
  design tokens (`hub.css` k9s aesthetic — dark, green accent
  `#a8ff7a`, compact density), `embed.FS` static handler.
- F2 list screens: `/investigations`, `/hosts`, `/runs`, `/collectors`,
  `/audit` redrawn — k9s tables (28px row, mono columns, dot status,
  severity badges, label chips). Investigations list gets filter chips
  + per-row finding mini-bars.
- F3 detail screens: `/hosts/<id>` (320px sidebar + main grid),
  `/runs/<id>` (5-col summary + per-host fan-out), `/settings`
  (LLM / Budgets / Storage cards + Tokens panel).
- F4 investigation detail: 3-column grid (timeline | findings |
  context), `framed` pulsing pending-card, real `EventSource` SSE
  reload on state-change, hypothesis hyp-card.
- Funcmap helpers: `compactNum`, `shortID`, `sinceUTC`, `pct`,
  `findCount`, `barRepeat`.
- `store.NavCounts()` + `store.FindingCountsByInvestigation()` for
  sidebar badges + mini-bars.

### Operator UX (`25c566d` `178a6d3`)

**Added**
- Inline login errors — bad creds re-render the form with an alert,
  no separate "401" page; username preserved.
- Bootstrap-token list + delete on `/settings`.
- Host actions on `/hosts/<id>`: **Revoke cert** + **Delete host**
  (the second refused while online).
- Scoped investigations: `/investigations` form gets agent-picker
  with all/none/online-only quick buttons. `allowed_hosts_json`
  per-investigation (migration 0009); tool handlers
  (`list_hosts` / `collect` / `collect_batch`) enforce server-side.
- `RECON_ADMIN_PASSWORD` plaintext accepted — hub bcrypt-hashes on
  startup. `RECON_ADMIN_PASSWORD_HASH` still wins when both set.

### Docker compose stack (`c617bb8` `24f018c`)

**Added**
- Multi-stage `Dockerfile` with two runtime targets (`hub-runtime`,
  `agent-runtime`), CGO_ENABLED=0, alpine runtime, non-root recon
  user.
- `docker-compose.yml`: hub (gRPC + UI) + nginx (TLS terminator,
  auto self-signed cert via `nginx-entrypoint.sh`, SSE-aware proxy
  block) + agent (compose profile `with-agent`).
- Make targets: `compose-up`, `compose-gen-hash`,
  `compose-gen-token`, `compose-bootstrap-agent`,
  `compose-rotate-ca`, `compose-reset`.
- `deploy/docker/{hub,agent,nginx}.{yaml,conf}` + `README.md` runbook.

### CI + release (`debd88c` `3c361c3` `3ebdf7f` `b49c5a9`)

**Added**
- `.github/workflows/ci.yml`: lint (golangci-lint v2.5.0 via action
  v7) + test (-race) + build smoke on every push/PR. Concurrency
  cancellation, Go module cache.
- `.github/workflows/release.yml`: on tag `v*` — artefacts
  (linux/amd64+arm64 hub & agent tarballs via `make dist`,
  SHA256SUMS, GitHub Release) + docker (buildx push to
  `ghcr.io/<repo>/recon-{hub,agent}`, semver+latest, multi-arch).
- Tarballs renamed `recon-{hub,agent}-linux-{amd64,arm64}.tar.gz`
  (no version in filename) so the install URL stays stable across
  releases.

### Quick install (`debd88c` `7c67d32` `142f986` `dd20839` `bb5d71a` `17f4932` `c038e29`)

**Added**
- `/install/agent.sh` endpoint serves a templated bash one-liner
  (single-use bootstrap token in URL = auth). Stop + wipe + reinstall
  path; downloads `recon-agent-linux-<arch>.tar.gz` from the
  configured release repo (`latest` or pinned tag); creates recon
  user with adm + systemd-journal supplementary groups + read-only
  caps; writes systemd unit + agent.yaml; starts service.
- "+ Quick install" form on `/hosts` flashes the curl one-liner with
  inline copy-to-clipboard.
- `install:` config: `release_repo_url`, `version`,
  `agent_grpc_endpoint` (`auto` or explicit), `grpc_port`,
  `external_url`, `trusted_tls`. All overridable via
  `RECON_INSTALL_*` env.

### Crashloop / bootstrap fixes (`72f1e70` `9bd238a` `e6f95d3` `b56babd` `47229fc` `fa5b29e`)

**Fixed**
- Agent deletes `bootstrap.token` after a successful Enroll.
- Agent refuses to call Enroll when both cert and token are
  missing — actionable "operator must issue a fresh token" error
  instead of crashlooping with PermissionDenied.
- Agent removes the on-disk token when Enroll RPC returns
  PermissionDenied — next start short-circuits to the no-token
  guard.
- `make compose-bootstrap-agent` revokes the prior identity on the
  hub before issuing a new token; `/install/agent.sh` quick-install
  handler does the same.
- gRPC dial gets a 15s `WaitForStateChange` cap — agent doesn't
  hang silently on TLS-handshake stalls; produces a clean
  `dial timeout to <endpoint>` error.
- Install script: stop+wipe always runs; journalctl scoped to
  `--since $START_TS` so we don't dump stale lines from a previous
  failed process.

### Networking + TLS fixes (`ab592ae` `b69e426` `989802a`)

**Fixed**
- Agent gRPC dialer pinned to `tcp4` — avoids ENETUNREACH from
  IPv6 happy-eyeballs on hosts with a configured-but-unusable v6
  default route.
- Hub regenerates the server leaf when `dns_names` / `ip_addrs` in
  `hub.yaml` change (symmetric drift — detects both added and
  removed SANs). CA stays put.

### Sessions (`0849b4d`)

**Added**
- Operator sessions persisted in SQLite (`web_sessions`, migration
  0010) — survive `docker compose up -d --force-recreate hub`.
  Login throttle + flash messages stay in memory by design.

### Security review fixes (`014b1e3`)

**Fixed**
- **H1** Login throttle XFF-spoofable — now only honors
  `X-Forwarded-For` when `auth.BehindTLSProxy` is true.
- **M1** Quick install silently kicked online agents — refuses
  with HTTP 409 when host is online; emits `identity.revoke`
  audit row whenever a prior enrolment is replaced.
- **M2** `curl -k` baked into every install one-liner — added
  `install.trusted_tls` flag; flips to verified `curl -fsSL` once
  a real CA cert fronts the hub.
- **M3** `/install/agent.sh` leaked deployment metadata to anyone
  — added `store.LookupBootstrapToken` (validate-only, no consume);
  bogus tokens get a flat 404.
- **M4** Server cert SAN drift detection was superset-only —
  symmetric check now also detects removed SANs.

---

## [Initial MVP — five weeks] — closed at `ebc0c5f`

### Week 5 — Auth, compaction, retention, packaging (`63ca84c`, `05abc13`)

**Added**
- **Auth + CSRF**: bcrypt password hash from env (`RECON_ADMIN_USER` /
  `RECON_ADMIN_PASSWORD_HASH`), server-side sessions (12h TTL,
  `crypto/rand` 256-bit sids), double-submit CSRF tokens with
  `subtle.ConstantTimeCompare`. `/login` and `/logout` pages.
- CLI helper `recon-hub --mode gen-password-hash` (reads
  `RECON_ADMIN_PASSWORD`).
- **Compaction**: when context exceeds ~150K tokens the loop folds the
  middle slice of the conversation into a `system_summary` message via a
  dedicated compaction prompt; preserves `system`+`goal`+last 8 messages.
- **Per-agent rate limit**: token-bucket per host (default 30 req/min,
  configurable via `hub.yaml` `runner.per_agent_rpm`); rate-limited
  tasks land as `status=undeliverable`.
- **Retention worker**: hourly sweep removes artifacts of finished tasks
  older than `storage.retention_days` and purges archived messages from
  closed investigations.
- **Settings page** (`/settings`): bootstrap-token issue + enrolled-host
  list. Tokens are shown via server-side flash (never in URL).
- **Audit filters**: `/audit?actor=&action=` `LIKE`-based filtering.
- **UI budgets**: investigation page shows progress bars
  steps used / max + tokens used / max.
- **Packaging**: hardened `deploy/systemd/recon-{hub,agent}.service`
  (ProtectSystem=strict, MemoryDenyWriteExecute, minimal
  CapabilityBoundingSet); `deploy/nginx/recon.conf` with TLS, HSTS, CSP,
  X-Frame-Options DENY, SSE-friendly upstream block; `deploy/docs/install.md`
  step-by-step runbook; Makefile `dist-hub` / `dist-agent` produces static
  linux/amd64+arm64 tarballs.
- Native go fuzzers for `parseSS`, `parseUnits`, `summarizeJournal`
  (~2M execs each, zero panics).

**Security (review fixes — `05abc13`)**
- Bootstrap tokens no longer transit the URL — flash store keyed off the
  session cookie (review C1).
- Compaction tokens accounted to a separate counter
  (`investigations.compaction_tokens`, migration 0008); user-visible budget
  gate ignores them (review C2).
- 10-minute cooldown after a failed compaction prevents budget burn on
  retries (review C3).
- Login brute-force throttle: 10 failures / 5 min sliding window per
  client IP (review H1).
- `RECON_BEHIND_TLS_PROXY=true` env makes `Secure` cookie aware of
  `X-Forwarded-Proto: https` (review H4).
- Compaction asserts bootstrap shape and wraps tool outputs in
  `<<<UNTRUSTED_HISTORY>>>` delimiters (reviews M10, M11).

### Week 4 — Operator control, audit, SSE, export (`55cf9ce`, `f2ae083`)

**Added**
- `Loop.Resume(ctx)` re-spawns advance() for `active` investigations on
  hub startup (review C4 from week 3).
- `InjectHypothesis`: discards the model's pending tool_call and appends
  an `OPERATOR HYPOTHESIS [priority: HIGH]` user message, forcing the
  next step to confirm or refute (PROJECT.md §7.5).
- `InjectIgnoreNote` / `InjectRestoreNote`: pin/ignore findings emit
  `system_note` directives the model honors / rescinds.
- `DecideWithEdit`: new `edit` decision overwrites pending tool_call
  input (validated as JSON object); `lastApproved` treats `edited` as
  approved.
- Broad-selector confirmation: `collect_batch` with >5 hosts re-flips to
  pending; second approve flips a typed flag (review C1) instead of a
  forgeable text marker.
- Web UI: pending tool_call card with JSON textarea + edit button;
  hypothesis form; pin/ignore buttons in findings table; `/audit` page;
  `/investigations/export/{id}` markdown download;
  `/investigations/events/{id}` SSE that emits a snapshot every second
  and triggers a JS `window.location.reload()` on state change.
- AuditLog wired into `investigation.{start,decide,hypothesis}`,
  `finding.{pin,unpin,ignore,unignore}`, `run.create`.

**Security (review fixes — `f2ae083`)**
- Replace forgeable broad-selector text marker with a typed
  `tool_calls.broad_confirmed` column (migration 0007, review C1).
- `InjectHypothesis` returns error on UPDATE failure instead of
  swallowing — no more deadlocked `pending` after a failed discard
  (review C2).
- `s.audit()` helper escalates AuditLog write failures to ERROR-level
  slog — audit cannot silently lose entries (review H2).
- `DecideWithEdit` enforces JSON-object shape (rejects `null`/scalars/
  arrays) so zero-valued struct fields can't slip past per-field
  validators (review H4).
- Resume aborts investigations missing system+user bootstrap (review M1).
- Ignore is idempotent; unignore emits a `RESTORED` system_note that
  rebuts the earlier IGNORED directive (reviews M3, M4).

### Week 3 — Investigator MVP via OpenRouter (`a5f9e26`, `320424c`, `3dea6e0`)

**Added**
- `internal/hub/llm`: thin OpenAI-compatible chat/completions client
  (function-calling tools). Default backend OpenRouter; URL/model/key
  from env (`RECON_LLM_BASE_URL`, `RECON_LLM_MODEL`,
  `RECON_LLM_API_KEY`). Hub starts without a key — investigator
  endpoints return 503 until configured.
- Migration 0005 — `investigations`, `messages`, `tool_calls`,
  `findings` (all FK CASCADE on parent delete).
- `internal/hub/investigator/prompt.go` — system prompt template adapted
  from BASE_TASKS.md §3 for OpenAI tool-calling (`tool_choice: "required"`
  replaces Anthropic's `{"type":"any"}`; extended thinking dropped —
  not portable across vendors).
- `internal/hub/investigator/tools.go` — 11 tool schemas
  (`list_hosts`, `list_collectors`, `describe_collector`, `collect`,
  `collect_batch`, `search_artifact`, `compare_across_hosts`,
  `get_full_result`, `add_finding`, `ask_operator`, `mark_done`).
- `Loop` driver: serialized per-investigation, enforces
  one-tool-call-per-turn, budgets `max_steps` / `max_tokens`.
- Web UI v1 for investigations (list + detail + Approve / Skip / End
  buttons).
- design.md sync (`3dea6e0`): `PROJECT.md` §10 / §4.2 / §7 / §11 / §13
  rewritten to describe the OpenAI-compat / OpenRouter transport that
  was actually built.

**Security (review fixes — `320424c`)**
- Migration 0006 + `messages.tool_calls_json` — assistant messages
  preserve their `tool_calls` so the next turn's `tool` message can
  anchor on its `tool_call_id` (review C1; otherwise OpenAI/OpenRouter
  rejects the second turn).
- Auto-approve narrowed to pure inventory tools (`list_hosts`,
  `list_collectors`, `describe_collector`); `add_finding`,
  `search_artifact`, `get_full_result`, `compare_across_hosts` now
  require operator approval — these emit data to the third-party LLM
  provider (review C2).
- `search_artifact`: 512-byte pattern cap, 4-MiB read cap, 5-second
  regex deadline in a goroutine with cancellable context — closes
  ReDoS / DoS surface (review C3).
- `llm.sanitizeForError` redacts our API key and provider key shapes
  (`sk-or-*`, `sk-ant-*`, `or-v*`) from non-2xx response bodies
  before they land in audit logs / UI (review H1).
- `store.GetTask(id)` direct lookup replaces an
  O(n_runs × n_tasks) walk in `getTask` and `taskTerminal` (review H5).

### Week 2 — Collectors, runs, exec gateway, UI (`3af5491`, `20caa1e`, `11a5c98`)

**Added**
- Agent runner with `recover()` around every collector call — exec
  gateway's intentional panic on disallowed (bin, args) cannot crash
  the agent (PROJECT.md §14).
- Exec gateway whitelist filled in (`systemctl`, `journalctl`, `ss`,
  `ip`, `iptables -L`); arg validators (`NoShellMeta`,
  `SystemdUnitName`, `JournalSince`, `PosInt`, etc.); sudoers template.
- Migration 0003 + hub-side runner (`runs`, `tasks`, `results`):
  `CreateRun` fans out CollectRequests via `api.SendCollect`,
  `api.ResultSink` interface delivers `OnResult` / `OnArtifact` to the
  runner, artifacts written to `{artifact_dir}/{task_id}/{name}`.
- 9 first-wave collectors (PROJECT.md §12): `system_info`, `dns_resolve`,
  `net_connect`, `net_ifaces`, `net_listen`, `systemd_units`,
  `journal_tail`, `process_list`, `file_read`, `disk_usage`.
- Web UI: `/hosts/{id}`, `/collectors`, `/runs`, `/runs/{id}`,
  POST `/runs/new`. Templates use unique block names; layout aliases
  `content` per render via `Clone()` to avoid name collision.

**Security (review fixes — `11a5c98`)**
- `closeOpenArtifacts` on `OnResult` prevents leaked `*os.File` handles
  when a stream ends without a `Last=true` chunk (review C1).
- Agent runner rejects duplicate `request_id` with `STATUS_ERROR` instead
  of overwriting the in-flight cancel (review C2).
- Migration 0004 rebuilds `tasks` with FK on `hosts(id) ON DELETE
  CASCADE`; `Open()` asserts `PRAGMA foreign_keys=1` at startup so a
  driver swap can't silently break cascade-delete (review C3).
- `file_read` refuses symlinks (Lstat + EvalSymlinks); the lexical
  denylist alone could be bypassed by a symlink inside an allowlist
  directory (review H1).
- `Entry.MaxStdoutBytes` (16 MiB cap for `journalctl`); exec gateway
  streams via `StdoutPipe` + `LimitedReader`; `ErrStdoutTruncated`
  surfaces as a `journal.truncated` hint instead of an error
  (review H2).
- `net_connect.disallowedTarget` blocks 169.254.169.254 / GCP-AWS-IPv6
  metadata endpoints, link-local, multicast, unspecified; max 16
  targets per call (review H4).

### Week 1 — Skeleton, mTLS, identity binding (`ebed445`, `11a5c98`)

**Added**
- gRPC `Hub` service (`Enroll` + bidi `Connect` stream); proto deliberately
  has no mutating verb (PROJECT.md §3.4 layer 1).
- Hub: SQLite store via `modernc.org/sqlite` (no CGO), self-signed CA
  bootstrap, mTLS gRPC server with `VerifyClientCertIfGiven` +
  per-method interceptor.
- Web UI v0: `/hosts` page (html/template + embed).
- Agent: yaml config, auto-facts, enroll-flow with one-shot
  `InsecureSkipVerify` protected by a bound bootstrap token, reconnect
  with jittered backoff, heartbeat, collector registry (compiled-in —
  PROJECT.md §3.4 layer 2).
- Exec gateway skeleton: empty whitelist + arg-shape validation, panics
  on any disallowed (bin, args) — PROJECT.md §3.4 layer 3.
- `system_info` collector — no exec, parses /proc and /etc/os-release.
- `.golangci.yml` v2 with `forbidigo` + `depguard` rules scoped to
  `internal/agent/collectors/**` banning mutating syscalls and direct
  `os/exec` import — PROJECT.md §3.4 layer 5.

**Security — identity binding (review fixes carried into `ebed445`)**
- `bootstrap_tokens` are bound to a single `expected_agent_id` at issue
  time (review C2).
- `enrolled_identities` table tracks `(agent_id, fingerprint)` with
  revoke; `Connect` verifies on every session (review C1).
- `Enroll` refuses re-enroll under an already-enrolled `agent_id`
  without explicit revoke (review C3).
- `recon-hub` CLI: `gen-token --agent-id <id>` (token bound to one id),
  `revoke --agent-id <id>`.

---

## Tooling and conventions

- Go 1.22+; single static binary per side (hub, agent), `CGO_ENABLED=0`.
- Lint: `golangci-lint v2` with custom `forbidigo` + `depguard` rules.
- Tests: `go test ./...` covers parsers, store CRUD, runner, exec
  gateway, llm client, identity lifecycle, broad-selector flag, etc.
- Fuzzing: `go test -fuzz=...` on `parseSS`, `parseUnits`,
  `summarizeJournal`.

## Deferred (post-MVP)

- testcontainers integration tests (Docker dependency).
- Streaming LLM responses (poll-based SSE is sufficient for current UX).
- Multi-user (MVP is single-operator).
- Local LLM via Ollama, sanitize-mode for PII, scheduled investigations
  with alert rules — explicitly out of scope per PROJECT.md §13.
