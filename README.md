# Recon

**Read-only diagnostic system for Linux fleets, driven by an LLM
investigator.** Operator types an incident as a goal — *"why are cronjobs
failing on the prod k8s cluster"* — and Recon walks the fleet step-by-step,
proposing observations the operator approves one at a time, until it
produces a structured post-mortem.

The whole stack is **read-only by construction** (PROJECT.md §3.4): no RPC,
collector, exec call, syscall, or library import in the critical path can
mutate target-host state. That makes it safe to run on production.

## Why

`kubectl exec`, `sosreport`, `must-gather`, `ansible -m shell` are
powerful but require either know-how, ad-hoc commands, or trust that
no one types `rm`. Recon is the opposite tradeoff:

- **Five layers of read-only enforcement** — protocol, compiled-in
  collector catalog, exec-gateway whitelist, OS-level capabilities,
  CI lint banning mutating imports/syscalls. The model and the operator
  literally cannot type a destructive command into the system.
- **Step-by-step under operator control** — every host-touching action
  is a card the operator clicks Approve on. Edit JSON params inline,
  Skip, End, or override with an Operator Hypothesis. The page updates
  live and advances to the next approval without a reload (server-pushed
  SSE from the investigator bus + a change-aware backstop poll; approve
  posts via `fetch` and swaps fragments in place — no HTMX, CSP allows
  only `script-src 'self'`).
- **Structured findings** — model can only emit findings backed by
  real `task_id` evidence; the post-mortem has citations.
- **Snapshot, not telemetry** — no continuous metrics; results are
  on-demand and stored only as long as you need them.

## What's in the box

- Single Go binary `recon-hub` (gRPC for agents + HTTP/HTML UI for
  operator + SQLite + LLM client).
- Single Go binary `recon-agent` (gRPC client + ten compiled-in
  read-only collectors).
- Defaults to **OpenRouter** as the LLM backend (`anthropic/claude-sonnet-4.5`
  out of the box). Any OpenAI-compatible chat/completions endpoint
  works — vLLM, LiteLLM, raw OpenAI.
- `deploy/` ready-to-install systemd units, hardened
  (`ProtectSystem=strict`, `MemoryDenyWriteExecute`, minimal
  `CapabilityBoundingSet`); nginx config (TLS termination, HSTS,
  CSP, SSE-friendly upstream); 0-to-investigation runbook in
  `deploy/docs/install.md`.
- 13 first-wave collectors covering the typical SRE first-five-minutes:
  `system_info`, `dns_resolve`, `net_connect`, `net_ifaces`, `net_listen`,
  `systemd_units`, `journal_tail`, `process_list`, `file_read`, `log_search`,
  `dmesg`, `disk_usage`. `file_read` returns the head, a byte `offset`, or the
  tail (`from_end` / `tail_lines`) of a file as a searchable artifact — so you
  can work backwards from a crash instead of from a 44 MB log's oldest line;
  `log_search` greps an RE2 regex across files **in-process on the host**
  (grep-at-source, with `since`/`until`), so megabytes never enter the model
  context; `journal_tail` reads a unit, the **kernel ring** (`kernel=true`),
  or the **previous boot** (`previous_boot=true`); `dmesg` reads the kernel
  ring with absolute ISO timestamps for time-anchoring.
- **CPU-first log retrieval** — large log artifacts are indexed and triaged
  deterministically on the hub (size, line count, time range, and
  severity/template clusters with line refs) so the investigator navigates
  via that index + targeted `search_artifact` instead of dumping raw bodies
  into the model context. No embeddings, rerankers, GPU, or CUDA are required;
  the default deployment is pure CPU/stdlib.

## Architecture (one paragraph)

```
Browser ── HTTPS ──▶ HUB (recon-hub)
                    ├── Web UI (HTML + SSE)
                    ├── Investigator (LLM loop, OpenAI-compat)
                    ├── Runner (per-host rate limit, cancel, retention)
                    ├── Store (SQLite: hosts, runs, tasks, results,
                    │          investigations, messages, tool_calls,
                    │          findings, audit, bootstrap_tokens)
                    ├── Auth (bcrypt + sessions + double-submit CSRF)
                    └── gRPC (mTLS) ──▶  Agent on every host
                                          (one-tool-call-per-turn,
                                           recover() around collectors,
                                           exec gateway with whitelist)
```

Full design: `DOCS/Prompts/PROJECT.md`. Prompt engineering for the
investigator: `DOCS/Prompts/BASE_TASKS.md`.

## Quickstart — docker compose (recommended)

```bash
cp .env.example .env
# fill in RECON_ADMIN_PASSWORD + RECON_LLM_API_KEY
make compose-up
```

UI is at **https://localhost:8443** (self-signed cert; accept once).
Hub bcrypt-hashes `RECON_ADMIN_PASSWORD` at startup. Prefer a
pre-computed hash (CI, config management)? Set
`RECON_ADMIN_PASSWORD_HASH` instead — it wins when both are set.

### Install an agent on any Ubuntu host

1. Log in at `/hosts`, click **+ Quick install**, type an `agent_id`
   (unique per host), click **Generate install link**.
2. Hub flashes a one-liner:
   ```
   curl -fsSLk "https://<hub>:8443/install/agent.sh?token=…&id=…" | sudo bash
   ```
   (Flips to verified `curl -fsSL` once you set
   `install.trusted_tls: true` — i.e. when the hub is fronted by a real
   CA-issued cert.)
3. Copy, send over SSH/DM, run as root on the target. The script
   downloads the matching binary (from GitHub Releases, or from the hub
   itself in self-hosted mode — see below), creates the `recon` user with
   read-only capabilities, writes systemd unit + config, starts the
   service. Agent appears online on `/hosts` within seconds.

### Self-hosted distribution — no GitHub

By default the agent ships via GitHub Releases (install, self-update, and
the "outdated" badge all hit GitHub). To run with **no GitHub access**, let
the hub serve the agent itself:

```bash
RECON_INSTALL_SELF_HOSTED=true \
RECON_VERSION=0.9.0 \
  docker compose up -d --build hub
```

- The hub image **bakes** `recon-agent-linux-{amd64,arm64}.tar.gz` +
  `checksums.txt` (built from the same commit, so the served agent matches
  the running hub) and serves them at `/releases/...` alongside a
  GitHub-API-shaped `GET /releases/latest` JSON.
- The Quick-install one-liner and the agent's self-updater are pointed at
  the hub (`<hub>/releases/...`) instead of GitHub; downloads are still
  SHA256-verified against the served `checksums.txt`.
- The **outdated badge** compares each host's reported version against the
  bundled agent version. Set `RECON_VERSION` to a real **semver** — a
  non-semver tag (including the `docker` default) collapses to `0.0.0` and
  silently disables version-skew detection.
- GitHub mode stays selectable: leave `RECON_INSTALL_SELF_HOSTED` unset (or
  `install.self_hosted: false` in `hub.yaml`).

### Local dev agent (same host)

```bash
make compose-bootstrap-agent
```
Starts a compose-network agent under the `with-agent` profile. Useful
for end-to-end testing without provisioning a separate host.

### Running bare-metal (no docker)

```bash
make build   # produces bin/recon-hub + bin/recon-agent

RECON_ADMIN_USER=admin \
RECON_ADMIN_PASSWORD='strong-password' \
RECON_LLM_API_KEY=sk-or-v1-... \
  ./bin/recon-hub --config ./deploy/dev/hub.yaml --mode serve

# in another terminal — issue a token + start an agent
TOKEN=$(./bin/recon-hub --config ./deploy/dev/hub.yaml --mode gen-token \
        --agent-id dev-agent-1 --token-ttl 1h)
echo "$TOKEN" > deploy/dev/state/agent/bootstrap.token
./bin/recon-agent --config ./deploy/dev/agent.yaml
```

Open `/hosts`, click **Investigations → new**, enter a goal, approve
the first step.

## Production deploy

See [`deploy/docs/install.md`](deploy/docs/install.md) for the full
runbook (systemd units, nginx in front, packaging tarballs). Highlights:

- TLS terminated by nginx; hub stays on `127.0.0.1`.
- `RECON_ADMIN_PASSWORD` (or pre-hashed `RECON_ADMIN_PASSWORD_HASH`) +
  `RECON_LLM_API_KEY`, LLM endpoint overrides, and hub IP SAN overrides live
  in `/etc/recon/hub.env` (`mode 0600`, `recon:recon`).
- `RECON_BEHIND_TLS_PROXY=true` so session cookies get the `Secure` flag
  even though hub itself speaks HTTP on the loopback.
- `recon-hub --mode revoke --agent-id <id>` rejects the cert at next
  Connect; re-enroll with a fresh bootstrap token.
- `make dist-hub dist-agent` builds static linux/amd64+arm64 tarballs
  with binary + systemd unit + relevant configs.

## Configuration

Hub (`/etc/recon/hub.yaml`):

```yaml
server:
  grpc_addr: ":9443"
  http_addr: "127.0.0.1:8080"
  dns_names: ["recon.example.com"]
  ip_addrs:  []

storage:
  db_path:        /var/lib/recon/recon.db
  artifact_dir:   /var/lib/recon/artifacts
  ca_dir:         /var/lib/recon/ca
  retention_days: 30

llm:
  # RECON_LLM_BASE_URL / RECON_LLM_MODEL from env override these defaults.
  base_url: https://openrouter.ai/api/v1     # any OpenAI-compatible endpoint
  model:    anthropic/claude-sonnet-4.5
  api_key_env: RECON_LLM_API_KEY
  allow_insecure_http: false                 # env: RECON_LLM_ALLOW_INSECURE_HTTP
  max_steps_per_investigation:  40
  max_tokens_per_investigation: 500000
  max_result_tokens:            2000         # cap per tool result; env: RECON_LLM_MAX_RESULT_TOKENS
  history_keep_recent_results:  6            # recent results kept verbatim; older → re-read pointers
  history_demote_min_bytes:     1024         # only demote tool results larger than this
  autodetect_context_window:    true         # probe GET /models for the real window when unset
  http_referer: https://recon.example.com    # OpenRouter ranking, optional
  x_title:      Recon                        # OpenRouter ranking, optional

runner:
  per_agent_rpm: 30                          # PROJECT.md §7.6
```

Env (`/etc/recon/hub.env`):

```
RECON_ADMIN_USER=admin
RECON_ADMIN_PASSWORD=strong-password
# or, for unattended setups: RECON_ADMIN_PASSWORD_HASH=<bcrypt hash>
RECON_LLM_API_KEY=sk-or-v1-...
RECON_LLM_BASE_URL=https://openrouter.ai/api/v1
RECON_LLM_MODEL=anthropic/claude-sonnet-4.5
RECON_LLM_ALLOW_INSECURE_HTTP=false
RECON_LLM_MAX_RESULT_TOKENS=2000           # optional; overrides llm.max_result_tokens
RECON_HUB_IP_ADDRS=127.0.0.1,<hub-lan-ip>
RECON_BEHIND_TLS_PROXY=true
RECON_LOG_LEVEL=info                        # debug|info|warn|error (hub AND agent); default info
```

`RECON_LOG_LEVEL` sets the structured-log verbosity for **both** binaries (hub and
agent); unset or unrecognized falls back to `info`. Raise it to `debug` to surface
the per-decision auto-approve trail (`[FIX:auto-approve] decision`) and other
diagnostics on prod without a rebuild — the Debug sites carry no secrets/PII. A
non-terminal probe held while automation is enabled is logged at `warn`, so the
"auto is on but it still asked me" case is visible even at the default level.

Operator-facing timestamps in the web UI render in the **browser's timezone**
(detected via `Intl`, stored in a `tz` cookie); all machine-facing surfaces — the
Markdown export, JSON API, SSE, the notebook, and every LLM-facing timestamp —
stay UTC.

`RECON_HUB_IP_ADDRS` is a comma-separated override for `server.ip_addrs`.
Changing SANs after `/var/lib/recon/ca/` has been generated requires
regenerating the CA/server leaf. The `resolved hub config` startup log exposes
non-secret effective fields: `llm_model`, `llm_base_url_scheme`,
`llm_base_url_host`, `llm_allow_insecure_http`, `server_ip_san_source`, and
`server_ip_san_count`.

### Troubleshooting LLM router 502s

If an investigation aborts with `llm chat: llm http 502`, first distinguish
the failing layer:

- nginx/reverse-proxy 502s appear in nginx access/error logs for paths such as
  `/investigations/...` or `/investigations/events/...`.
- LLM router 502s appear in the hub log as an `investigator step` error with
  the upstream body from `RECON_LLM_BASE_URL`.

For router errors like `No tool call found for function call output`, Recon
drops orphaned tool-result messages before calling the LLM and logs
`dropped orphan tool results ...` with the investigation ID and count. If 502s
continue, check the external router logs for the same timestamp and verify the
configured model name is accepted by that router.

### Continuing aborted investigations

An aborted investigation can be continued only when the hub has a live LLM
client. After changing LLM config, restart the hub and confirm startup logs:
`LLM client ready` for enabled state, or `LLM client disabled` with
`reason_class`, `llm_model`, `llm_base_url_scheme`, and
`llm_base_url_host` for disabled state.

Expected UI states:

- `aborted` + LLM enabled: the detail page shows `Continue investigation`.
- `aborted` + LLM disabled: the detail page shows the disabled recovery panel
  with `RECON_LLM_API_KEY`, `RECON_LLM_BASE_URL`, and `RECON_LLM_MODEL`.
- `done` + LLM enabled: the detail page shows `Continue investigation` — a
  completed run can be reopened **in place** (status returns to `active` and an
  `OPERATOR RESUME` turn is appended). The prior conclusion is preserved and
  handed to the model as a starting point to extend or revise, not re-derive.
- `done` + LLM disabled: a panel explaining continuation needs a configured LLM
  client and a hub restart.

If a browser POST to `/investigations/continue` ever shows plaintext
`investigator disabled`, the deployment is stale. Update the hub, restart it,
and recheck the startup fields above.

Agent (`/etc/recon/agent.yaml`):

```yaml
hub:
  endpoint: "recon.example.com:9443"
  ca_cert: /var/lib/recon/hub-ca.pem
  cert:    /var/lib/recon/agent.pem
  key:     /var/lib/recon/agent.key
  bootstrap_token: /var/lib/recon/bootstrap.token
  server_name: recon.example.com

identity:
  id: prod-app-01
  labels:
    env: prod
    role: app

runtime:
  max_concurrent_collectors: 4
  artifact_dir: /var/lib/recon/artifacts
  default_timeout: 30s
  heartbeat_interval: 15s
```

## Operator controls — driving an investigation

Everything the operator can do from the investigation detail page
(`/investigations/<id>`). The page updates live (no reload): each action swaps
the affected regions in place.

**Approve / steer a proposed step** — when the model proposes a host-touching
tool call it appears as a **Pending approval** card in the centre timeline:

- **Approve** — run it as proposed.
- **Edit & Approve** — tweak the call's input JSON in the textarea, then run it
  (surgical change to that one action).
- **Skip** — drop this step; the model plans the next one.
- **End investigation** — stop here and close the run.
- **⚡ auto** (status header) — toggle auto-approve so every *subsequent* tool
  call runs without a click. Toggle it off to return to step-by-step. A
  model-proposed `mark_done` is **never** auto-closed by this toggle alone — it
  still waits for your confirmation (unless you explicitly finalized the run via
  *Stop · summarise*), so an auto-approved investigation can't close itself out
  from under you.
- **▶ run autonomously** (status header) — a *bounded* version of ⚡ auto: set a
  step and/or token budget and the model runs probes with no per-step
  confirmation until that budget is spent, then **pauses for your review** (it
  does not abort, and does not silently keep going). Use **⏸ take over** to
  hand control back to step-by-step at any time; re-arm another burst from the
  paused card. The same `mark_done` review rule applies — a conclusion still
  surfaces for you unless you finalized the run.

**Add a new thought / redirect — "⚠ Direct the model"** (right sidebar, shown
while the run is active / waiting / paused). This is the free-text channel for
injecting your own hypothesis mid-investigation:

- **Claim** (required) — the observation or hypothesis you want pursued.
- **Expected evidence** — what would confirm or refute it.
- **Instruction** — what the model should do about it.
- **Inject** — **replaces the current pending step** and forces the model to
  verify your claim before continuing (a MUST-level `OPERATOR HYPOTHESIS
  [priority: HIGH]`). Use this to steer, not to leave a side comment — it
  discards the model's pending proposal.

> There is intentionally no plain "chat" box during an active run: operator
> input is either a per-step decision (Approve/Edit/Skip/End) or a Direct-the-model
> injection. The model can also ask *you* a question — an `ask_operator` step
> shows up as a dedicated **"Model is asking"** card with the question and an
> answer box; type your reply and hit **Send answer**. Your answer is delivered
> back to the model as that tool call's result and the investigation resumes
> (you can also **Skip question** to let it proceed without one).

**When the budget caps out (paused)** — the timeline shows a **Budget
exhausted** card:

- **+ 500k tokens · continue** — extend this investigation's budget and resume.
- **Stop · summarise + "look here next"** — finalize with a conclusion plus
  next-step hypotheses, even though the budget is spent.

**Recover an aborted run** — for `aborted` investigations (LLM enabled):

- **Retry last step** — re-send the *same* request for a transient LLM failure
  (network / 5xx / rate-limit); no operator message is added.
- **Continue investigation** — a free-text box that reopens the conversation
  with your message (appends an `OPERATOR RESUME` turn). See
  [Continuing aborted investigations](#continuing-aborted-investigations).

A **`done`** investigation can be reopened the same way: open it and use
**Continue investigation** to return it to `active` with your follow-up. The
original conclusion stays in the timeline and is fed back to the model as a
starting point, so you don't re-walk the path you already covered.

**Curate findings** (right panel) — **pin** a finding to keep it prominent, or
**ignore** it to permanently close that branch of the investigation.

## Investigation memory & model routing

How a long investigation stays grounded, bounded, and explainable.

**Cross-investigation priors.** A new investigation is automatically seeded with
a compact, host-scoped digest of conclusions from prior **`done`** investigations
on overlapping hosts — so you don't re-walk a path an earlier run already
covered. The digest is bounded (a few investigations × a few findings, with a
hard token cap) and injected as a read-only `system` note labeled **"HINTS, not
established facts — re-verify"**. The evidence-first rule still applies, so a
prior conclusion can never become a finding on its own: every `add_finding`
evidence ref must resolve to a task in *this* investigation. Which priors were
attached is shown in the **Prior investigations** panel on the detail page, and
you can attach specific runs by hand (on top of the automatic host-scoped ones)
via the **Attach prior investigations** picker on the new-investigation form.
That picker lists **done, aborted, and active** runs — a done run attaches its
conclusion, an aborted/active run attaches its findings/partial evidence — while
the *automatic* host-scoped selection stays done-only. Tune it under
`investigator.priors` in `hub.yaml`:

| key | default | meaning |
| --- | --- | --- |
| `enabled` | `true` | inject the priors digest at the start of a new investigation |
| `max_investigations` | `4` | cap how many prior investigations the digest names |
| `max_findings_per_investigation` | `3` | cap findings shown per prior |
| `scope` | `host_overlap` | `host_overlap` = only priors sharing ≥1 host (fleet-wide runs fall back to recency); `recent_any` = most recent regardless of host |
| `max_age_days` | `30` | skip priors older than this |

**Why an investigation aborted.** Terminal state is first-class: every
abort carries a typed payload (`kind`, `reason`, `recoverable`, `source`).
The detail page shows an *Aborted reason* panel above the continue form
(recoverable aborts prefill a continue hint); the Markdown export and
`GET /api/v1/investigations/{id}` expose `terminal_kind` / `terminal_reason`
/ `terminal_recoverable` / `terminal_source`.

**Context window & compaction.** `llm.max_tokens_per_investigation` is a
spend budget, *not* the model context window. The per-profile
`context_window_tokens` is the real window; compaction triggers at **50%** of
it (output reserve + safety headroom subtracted before each call), so the
prompt never silently overflows. Per-turn accounting is persisted
(`investigation_context_turns`). When you leave `context_window_tokens` unset,
the hub does a **best-effort auto-detect** at startup (`GET {base_url}/models`
→ OpenRouter `context_length` / vLLM `max_model_len`); raw OpenAI exposes no
window field, so a conservative compile-time fallback (200K for the default
sonnet, 128K otherwise) is used and flagged with a `WARN` + a `fallback` badge
on `/settings`. Auto-detect never overrides an explicit config value and can be
disabled with `llm.autodetect_context_window: false`.

**Recovering an aborted run.** A transient LLM failure (network / 5xx /
rate-limit) marks the abort `transient` and the detail page offers a one-click
**Retry last step** — it re-sends the *same* request, injecting no operator
turn. Aborts that need redirection (operator ended early, etc.) use the
free-text **Continue** form below it, which appends an `OPERATOR RESUME` turn.

**Durable memory + notebook.** Compaction does not just hide old messages —
it writes a durable `investigation_memory` record and keeps a short
`system_summary` in live context that references it by `memory_id`. Each
`add_finding` likewise writes a `kind=finding` memory record (with the cited
`task_id`s) and a system note telling the model to cite
`finding_id` / `memory_id` / `task_id` after compaction. A human-readable
**notebook** is written per investigation at
`<artifact_dir>/investigations/<investigation_id>/notebook.md` (goal, model,
findings, conclusion, operator hypotheses, compaction memory) — linked from
the detail page (“Notebook”), the Markdown export, and the API
(`notebook_path`, `memory_count`). Retention keeps it while the investigation
is live or within the window, and removes it with the investigation.

**Large-log retrieval (CPU-first).** Log artifacts are indexed and triaged
deterministically (size, line count, time range, severity/template clusters
with line refs) — surfaced as `artifact_index` in tool results. The model is
told to navigate via the index + targeted `search_artifact` and to cite line
refs; the hub rejects oversized `get_full_result` (>200 KiB, `"force": true`
to override after a search) and caps repeated identical searches. No
embeddings/GPU/CUDA — see Task 10A note above.

**Work backwards from the incident.** `file_read` now emits its window as a
searchable artifact (the inline result is metadata only), and `search_artifact`
scans the first 4 MiB of an artifact (it sets `file_truncated`); for larger
files the model is told to grep at the source with `log_search` or read the
tail with `file_read(from_end)`, never to re-read the same path at a larger
`max_bytes`. The system prompt anchors the incident in time —
`boot ≈ system_info.collected_at − uptime_sec`, not wall-clock "now" — and
works backwards via `log_search(since/until)`, `journal_tail(previous_boot)`,
and the kernel ring (`journal_tail(kernel=true)` / `dmesg`); an empty
`journal_tail` (volatile journald) routes the model to `log_search`/`file_read`
over `/var/log`. Two anti-loop guards back this up: a redundant `file_read`
(same path/region differing only in `max_bytes`) is blocked, and after three
consecutive `ask_operator` calls with no new evidence the model is nudged to
derive host-answerable facts itself or close `inconclusive`. Reading the kernel
ring on hardened hosts (`kernel.dmesg_restrict=1`) needs `CAP_SYSLOG`, granted
in `deploy/systemd/recon-agent.service`.

**Token economy for huge log arrays.** Beyond the CPU index above, four
deterministic layers keep fleet-wide log surveys within budget:

- **Per-result cap** (`llm.max_result_tokens`, default 2000; env
  `RECON_LLM_MAX_RESULT_TOKENS`). Every `collect` / `collect_batch` /
  `search_artifact` result is held to this token budget. When over, the result
  is demoted in steps — shorten cluster examples → drop line samples → collapse
  each `artifact_index` to a headline → drop whole tasks — emitting
  `_index_truncated` / `_omitted_tasks` / `omitted_matches` / `_hint`. Full data
  always stays behind `get_full_result(task_id)` / `search_artifact`.
- **Cross-host roll-up.** A `collect_batch` over one log collector merges
  per-host clusters by (template, severity) into a single `batch_rollup` with a
  bounded per-host breakdown (host_id + task_id + line refs) instead of
  repeating each host's patterns. A 50-host survey becomes one bounded result
  while per-host drill-in via `search_artifact` still works.
- **History demotion** (`llm.history_keep_recent_results` = 6,
  `llm.history_demote_min_bytes` = 1024). Older bulky tool results in the live
  context are collapsed to one-line `RESULT task_id=… — re-read…` pointers
  *on the wire only* — the stored history stays full — pushing the LLM
  compaction later. `add_finding` / `ask_operator` results are always kept
  verbatim.
- **Token-estimate calibration.** The compaction trigger's bytes/token ratio
  self-corrects per investigation from the provider's reported `prompt_tokens`
  (EWMA, clamped `[2.5, 6.0]`), so the trigger fires neither early nor late on
  log-dense JSON. Surfaced as `token_calibration_ratio` in the API.

**Prompt caching.** On a profile with `supports_prompt_cache: true` (e.g.
OpenRouter → Anthropic models) the hub marks a `cache_control` breakpoint on the
byte-stable system-prompt prefix so it is served from cache instead of re-billed
every turn; OpenAI caches that prefix automatically. Leave it **off** for
OpenAI/vLLM or any gateway that rejects unknown content fields — the wire format
is then byte-identical to plain chat. Effectiveness is reported as
`cached_tokens` in the investigation API.

**Model profiles & routing.** With no `profiles:` block the single configured
model handles everything (backward-compatible). When profiles are set, the
router picks one per operation by role and falls back to primary:
`plan_next_step→primary` (tool-capable), `compact_memory→summarizer`,
`log_triage_summary→cheap`, `verify_finding`/`final_summary→verifier`. It
retries transient 429/5xx/network with backoff and switches to the primary
profile once before giving up. A new investigation can pin a specific profile
(form selector / `model_profile` in the API; empty = auto). Three shapes —
single-model, primary+cheap summarizer, and primary+summarizer+verifier — are
shown commented in [`deploy/docker/hub.yaml`](deploy/docker/hub.yaml).

**Safe log fields.** When debugging, inspect `investigation_id`,
`terminal_kind`, `terminal_reason`, `model_profile`, `context_window_tokens`,
`estimated_prompt_tokens`, `provider_prompt_tokens`, `threshold_tokens`,
`memory_id`, and the token-economy metrics: `max_result_tokens`,
`final_tokens` / `omitted_tasks` (result budget), `demoted_count` (history
demotion), `cached_tokens` / `cache_read_tokens` and `breakpoints_set` (prompt
cache), `observed_ratio` / `ewma_ratio` (calibration), and `result_tokens`
(search cap). **Never log** prompts, API keys, cookies, `Authorization`
headers, or raw log artifacts — the hub sanitizes provider error bodies before
they reach logs or the UI.

### Troubleshooting

- **"It aborted again right after I continued."** Read the *Aborted reason*
  panel first. A `kind=llm_error` with an upstream 5xx/502 is usually the
  router/provider, not Recon — see *Troubleshooting LLM router 502s* above.
  Continue retries the transient failure from the last good evidence.
- **"Context budget exhausted / budget pause."** That is the spend budget
  (`max_tokens_per_investigation`), not the context window. Use **Extend** to
  buy another slice, or **Finalize** to force a `mark_done` from the evidence
  already on the timeline. Compaction (at 50% of the context window) is
  separate and charged to its own counter.

## Security model

- **Read-only by construction** — five enforcement layers, see
  PROJECT.md §3.4. No collector or RPC can write/delete/exec-mutate.
- **mTLS for agent ↔ hub** — self-signed CA bootstrapped on first hub
  start; agents enroll with a one-shot token bound to a single
  `agent_id`; subsequent re-enroll requires explicit revoke.
- **Identity binding** — `Connect` verifies `(agent_id, cert_fingerprint)`
  against `enrolled_identities` on every session; revoked or stolen
  certs get `agent identity revoked` + audit row.
- **Operator auth** — bcrypt password from env; server-side sessions
  with `crypto/rand` 256-bit sids; double-submit CSRF tokens with
  `subtle.ConstantTimeCompare`; `SameSite=Strict` cookies; brute-force
  throttle (10 fails / 5 min / IP).
- **LLM transport** — refuses plaintext `http://` to non-loopback
  base URLs unless `RECON_LLM_ALLOW_INSECURE_HTTP=true` is set for a
  private/link-local router IP; public plaintext provider URLs remain
  rejected. Non-2xx response bodies are sanitized before surfacing errors;
  tokens stay in env, never in `hub.yaml`.
- **Operator-only data egress to provider** — `add_finding`,
  `search_artifact`, `get_full_result`, `compare_across_hosts`,
  `collect{,_batch}`, `mark_done`, `ask_operator` all require
  per-step Approve. Only pure-inventory tools (`list_hosts`,
  `list_collectors`, `describe_collector`) auto-execute.
- **Audit log** — every operator action and every loop-side override
  recorded; survives crashes via slog escalation; filterable in UI.
- **Sandboxing** — systemd hardening (`ProtectSystem=strict`,
  `MemoryDenyWriteExecute`, minimal `CapabilityBoundingSet`); nginx
  with HSTS, CSP, X-Frame-Options DENY.

Five rounds of security review with all Critical and the High items
relevant to MVP closed. See `CHANGELOG.md` for the per-week details.

## Development

```bash
make tools          # install protoc-gen-go and protoc-gen-go-grpc
make proto          # regenerate gRPC stubs
make build          # build hub + agent
make test           # unit tests
make lint           # golangci-lint v2 with custom forbidigo+depguard rules

# Fuzzers (zero panics across ~2M execs each in CI):
go test -fuzz=FuzzParseSS         -fuzztime=10s ./internal/agent/collectors/net/
go test -fuzz=FuzzParseUnits      -fuzztime=10s ./internal/agent/collectors/systemd/
go test -fuzz=FuzzSummarizeJournal -fuzztime=10s ./internal/agent/collectors/systemd/
```

## Status

MVP closed; productionisation track ongoing. Post-MVP highlights:

- **Docker compose stack** — hub + nginx (TLS termination) + optional
  local agent. `make compose-up` is the recommended path.
- **GitHub Actions release pipeline** — on tag `v*`, builds
  linux/amd64+arm64 tarballs + pushes `ghcr.io/<repo>/recon-{hub,agent}`
  multi-arch images.
- **Quick install** — one-liner served by the hub on `/install/agent.sh`;
  auto-detect arch, pulls the matching release tarball, sets up
  systemd unit with read-only caps.
- **Self-hosted distribution** — the hub can bake + serve the agent
  tarballs/checksums and a `releases/latest` JSON from `/releases`, so
  install, self-update, and the outdated badge work with no GitHub access
  (`RECON_INSTALL_SELF_HOSTED=true`).
- **k9s UI redesign** — dark, green accent, compact density; live SSE
  on the investigation detail screen.
- **Post-MVP security review** — six fix commits closing one High and
  four Medium findings since the MVP shipped.

See `CHANGELOG.md` for the full diary and `DOCS/Prompts/PROJECT.md` for
the design.

## License

Not yet decided (pre-1.0). The design documents in `DOCS/` are authored
by the project owner.
