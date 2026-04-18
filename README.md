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
  Skip, End, or override with an Operator Hypothesis.
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
- 11 first-wave collectors covering the typical SRE first-five-minutes:
  `system_info`, `dns_resolve`, `net_connect`, `net_ifaces`, `net_listen`,
  `systemd_units`, `journal_tail`, `process_list`, `file_read`,
  `disk_usage`.

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

## Quickstart (single VM, 5 minutes)

Build:

```bash
make build              # produces bin/recon-hub and bin/recon-agent
```

Set up the hub:

```bash
sudo install -d -m 0750 -o $USER -g $USER /var/lib/recon
mkdir -p deploy/dev/state/agent

# Issue a bootstrap token bound to one agent_id.
TOKEN=$(./bin/recon-hub --config ./deploy/dev/hub.yaml --mode gen-token \
        --agent-id dev-agent-1 --token-ttl 1h)
echo "$TOKEN" > deploy/dev/state/agent/bootstrap.token

# Start the hub WITH the LLM (replace with your OpenRouter key). The hub
# bcrypt-hashes RECON_ADMIN_PASSWORD at startup. For unattended setups
# (CI, config management) you can pre-compute and pass RECON_ADMIN_PASSWORD_HASH
# instead — it wins over the plaintext when both are set.
RECON_ADMIN_USER=admin \
RECON_ADMIN_PASSWORD='strong-password' \
RECON_LLM_API_KEY=sk-or-v1-... \
  ./bin/recon-hub --config ./deploy/dev/hub.yaml --mode serve
```

In another terminal:

```bash
./bin/recon-agent --config ./deploy/dev/agent.yaml
```

Open <http://127.0.0.1:8080>, log in, you should see the agent online.
Click **Investigations → new**, enter a goal, approve the first step.

## Production deploy

See [`deploy/docs/install.md`](deploy/docs/install.md) for the full
runbook (systemd units, nginx in front, packaging tarballs). Highlights:

- TLS terminated by nginx; hub stays on `127.0.0.1`.
- `RECON_ADMIN_PASSWORD` (or pre-hashed `RECON_ADMIN_PASSWORD_HASH`) +
  `RECON_LLM_API_KEY` live in `/etc/recon/hub.env` (`mode 0600`,
  `recon:recon`); never in `hub.yaml`.
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
  base_url: https://openrouter.ai/api/v1     # any OpenAI-compatible endpoint
  model:    anthropic/claude-sonnet-4.5
  api_key_env: RECON_LLM_API_KEY
  max_steps_per_investigation:  40
  max_tokens_per_investigation: 500000
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
RECON_BEHIND_TLS_PROXY=true
```

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
  base URLs; sanitizes API keys from non-2xx response bodies before
  surfacing errors; tokens stay in env, never in `hub.yaml`.
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

MVP closed. Five feature weeks shipped, eleven commits including five
security-review fix commits. See `CHANGELOG.md` for the full diary and
`DOCS/Prompts/PROJECT.md` for the design.

## License

Not yet decided (pre-1.0). The design documents in `DOCS/` are authored
by the project owner.
