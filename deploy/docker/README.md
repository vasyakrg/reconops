# Recon hub via docker compose

Three services live in `docker-compose.yml`:

| service | always-on | what it does |
|---|---|---|
| `hub`   | yes | gRPC for agents (`:9443`), HTTP UI on the internal compose network only |
| `nginx` | yes | TLS terminator + reverse proxy fronting `hub:8080` (auto self-signed cert on first start) |
| `agent` | profile `with-agent` | Local recon-agent for end-to-end dev, connects to `hub:9443` over mTLS |

Real production agents run on the target hosts via the systemd unit
(`deploy/systemd/recon-agent.service`), not in compose. The `agent`
service is a convenience for local smoke testing the whole pipeline.

> **No GPU required.** Log retrieval (artifact indexing + triage) is
> deterministic and CPU-only — no embeddings, rerankers, or CUDA. The hub
> image is a plain static Go binary; do not provision GPU nodes for Recon.

## 1. Bootstrap

```bash
cp .env.example .env
```

Set `RECON_ADMIN_PASSWORD=strong-password`, `RECON_LLM_API_KEY=sk-or-v1-…`,
and `RECON_HUB_IP_ADDRS=<hub-ip>[,<extra-ip>...]` in `.env`. The hub
bcrypt-hashes the plaintext at startup — no separate `gen-password-hash`
step required.

If you'd rather hand out a pre-computed hash (CI, config management, etc.),
set `RECON_ADMIN_PASSWORD_HASH` instead — it wins over the plaintext when
both are set:

```bash
make compose-gen-hash PASSWORD='strong-password'
```

> **Production note:** edit `deploy/docker/hub.yaml` *before* the first start to
> add the hub's real DNS name(s) under `server.dns_names`, and set the hub's
> real IP SANs in `.env` with `RECON_HUB_IP_ADDRS`. The bootstrap CA bakes them
> into the server cert and changing them later means regenerating
> `/var/lib/recon/ca/`.
>
> Set `RECON_TLS_CN=<your hostname>` in `.env` so the nginx self-signed cert
> matches. Or bind-mount your real cert at `/etc/nginx/certs/server.{crt,key}`.

LLM endpoint overrides are also env-first: set `RECON_LLM_BASE_URL` and
`RECON_LLM_MODEL` in `.env`. If the base URL is a private/link-local
`http://` router IP, explicitly set `RECON_LLM_ALLOW_INSECURE_HTTP=true`;
public plaintext provider URLs stay rejected.

**Huge log arrays / token economy.** `llm.max_result_tokens` (env
`RECON_LLM_MAX_RESULT_TOKENS`, default 2000) caps the per-tool-result tokens the
model sees; fleet `collect_batch` surveys are rolled up across hosts and aged
results are demoted to re-read pointers automatically. Tune
`history_keep_recent_results` / `history_demote_min_bytes` in `hub.yaml`, and
set `supports_prompt_cache: true` on a profile only when the backend honours
Anthropic-style `cache_control` (OpenRouter → Anthropic). See the README
“Token economy for huge log arrays” section.

On startup, inspect the `resolved hub config` log line for non-secret effective
values: `llm_model`, `llm_base_url_scheme`, `llm_base_url_host`,
`llm_allow_insecure_http`, `server_ip_san_source`, and
`server_ip_san_count`.

## 2. Up

```bash
make compose-up           # builds + starts hub + nginx
make compose-logs
```

The UI is on **<https://localhost:8443>** (browser will warn about the self-signed
cert — accept). gRPC for agents on `localhost:9443`.

> **Firewall:** on a multi-VM setup, agents on remote hosts dial port `9443`
> directly (not through nginx). If you've enabled ufw / iptables / nftables on
> the hub host, open the port:
> ```bash
> sudo ufw allow 9443/tcp && sudo ufw reload
> ```
> A "connect: network is unreachable" error in the agent's journal means
> something between agent and hub is rejecting `9443` with ICMP-unreachable —
> almost always a host-level firewall on the hub VM.

## 3. Local agent (optional)

For end-to-end dev — runs a recon-agent inside compose so you can poke at the
investigator without provisioning a real host:

```bash
make compose-bootstrap-agent
```

This:
1. issues a 1h bootstrap token via `compose exec hub …`,
2. seeds it into the `recon-agent-state` volume,
3. starts the `agent` service under profile `with-agent`.

Within ~5s the agent appears as `local-compose-agent` on the **/hosts** page.

To stop just the agent: `docker compose --profile with-agent down agent`.

## 4. Issue a bootstrap token for a real (off-compose) agent

Tokens are bound to a single `agent_id` and shown only once.

```bash
make compose-gen-token AGENT_ID=prod-app-01 TTL=24h
```

Copy the token, install the agent on the target host
(see `deploy/docs/install.md`), write the token into
`/var/lib/recon/bootstrap.token`, start `recon-agent`.

## 5. Revoke an agent

```bash
docker compose exec hub /usr/local/bin/recon-hub \
  --config /etc/recon/hub.yaml --mode revoke \
  --agent-id prod-app-01 --revoke-reason "host decommissioned"
```

The next `Connect` is rejected. To re-enrol, issue a fresh bootstrap token.

## 6. Backups & state

Three named volumes:

* `recon-state`       — db, artifacts, generated CA, agent identities (the only
                        one you actually need to back up)
* `nginx-certs`       — the auto-generated self-signed cert; regenerable
* `recon-agent-state` — local agent's client cert + bootstrap token; wipe to
                        re-enrol the local agent

Snapshot the important one:

```bash
docker run --rm -v recon-state:/var/lib/recon -v "$PWD":/backup alpine \
  tar czf /backup/recon-state-$(date +%F).tar.gz -C /var/lib/recon .
```

## 7. Upgrade

```bash
git pull
docker compose build --pull          # rebuilds hub + agent (shared builder stage)
docker compose up -d
```

The hub re-applies SQLite migrations on start; the volume's data carries over.

## 8. Troubleshoot 502s

Separate reverse-proxy 502s from LLM-router 502s before changing nginx:

- `docker logs recon-nginx` shows reverse-proxy status codes for browser paths
  such as `/investigations/...` and `/investigations/events/...`.
- `docker logs recon-hub` shows LLM upstream failures as `investigator step`
  errors, for example `llm chat: llm http 502: ...`.

If the upstream body says `No tool call found for function call output`, the
hub now filters orphaned tool-result messages before the LLM request and logs
`dropped orphan tool results ...` with the investigation ID. Continued 502s at
the same timestamp should be investigated in the external router behind
`RECON_LLM_BASE_URL`; also confirm `RECON_LLM_MODEL` is a model name that the
router actually serves.

## 9. Smoke aborted-investigation continuation

Continuing an aborted investigation requires a live LLM client and a hub
restart after config changes. Confirm one of these startup log states first:
`LLM client ready`, or `LLM client disabled` with `reason_class`,
`llm_model`, `llm_base_url_scheme`, and `llm_base_url_host`.

Expected UI states:

- `aborted` + LLM enabled -> `Continue investigation` form.
- `aborted` + LLM disabled -> disabled recovery panel naming
  `RECON_LLM_API_KEY`, `RECON_LLM_BASE_URL`, and `RECON_LLM_MODEL`.
- `done` + LLM enabled -> `Continue investigation` reopens the completed run in
  place (status returns to `active`, prior conclusion preserved).

Smoke a deployed container with an authenticated browser session:

```bash
RECON_SMOKE_BASE_URL=https://127.0.0.1:8443 \
RECON_SMOKE_INVESTIGATION_ID=inv_... \
RECON_SMOKE_COOKIE_HEADER='recon_sid=...; recon_csrf=...' \
RECON_SMOKE_CSRF_TOKEN='...' \
RECON_SMOKE_EXPECT=disabled \
  scripts/smoke/investigation-continue.sh
```

Run again with `RECON_SMOKE_EXPECT=enabled` after pointing
`RECON_LLM_BASE_URL` at a working OpenAI-compatible endpoint and restarting the
hub. The script prints request path, HTTP status, and high-level state; do not
paste `.env`, API keys, or Authorization headers into smoke output. If it
reports plaintext `investigator disabled`, update/restart the hub and inspect
the startup log fields above.
