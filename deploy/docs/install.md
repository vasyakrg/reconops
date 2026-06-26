# Recon — install guide

Production install on Ubuntu 22.04 / Debian 12 / RHEL 9. Hub on one VM,
agents on each target host. Goal: ≤10 minutes to first investigation.

## 0. Prerequisites

- One VM for the **hub** (1 vCPU, 1 GiB RAM, 5 GiB disk per ~50 agents).
- A reachable hostname / TLS cert (Let's Encrypt) — only if exposing UI
  beyond the local network.
- An OpenRouter API key (or any OpenAI-compatible endpoint) for the
  investigator.
- One `recon` system user on every host (hub + agents).

```bash
sudo useradd --system --create-home --shell /usr/sbin/nologin recon
sudo install -d -m 0750 -o recon -g recon /var/lib/recon /etc/recon
```

## 1. Hub install

```bash
# Place the binary.
sudo install -m 0755 recon-hub /usr/local/bin/

# Config + env file (env holds secrets and operator-facing deploy overrides).
sudo install -m 0640 -o recon -g recon hub.yaml /etc/recon/hub.yaml
sudo install -m 0600 -o recon -g recon hub.env  /etc/recon/hub.env
```

`hub.env` minimal contents:

```
RECON_LLM_API_KEY=sk-or-v1-...
RECON_LLM_BASE_URL=https://openrouter.ai/api/v1
RECON_LLM_MODEL=anthropic/claude-sonnet-4.5
RECON_LLM_ALLOW_INSECURE_HTTP=false
RECON_HUB_IP_ADDRS=127.0.0.1,<hub-lan-ip>
RECON_ADMIN_USER=admin
RECON_ADMIN_PASSWORD_HASH=<see below>
```

Generate the password hash:

```bash
sudo RECON_ADMIN_PASSWORD='strong-password' \
  /usr/local/bin/recon-hub --mode gen-password-hash
```

Install systemd unit:

```bash
sudo install -m 0644 deploy/systemd/recon-hub.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now recon-hub
sudo systemctl status recon-hub
```

The first start generates a self-signed CA in `/var/lib/recon/ca/` plus
the server cert. Logs (`journalctl -u recon-hub`) print the listening
addresses.

`RECON_HUB_IP_ADDRS` overrides `server.ip_addrs` from `hub.yaml` and is the
preferred place to manage IP SANs. Changing DNS or IP SANs after the CA/server
leaf has been generated requires regenerating `/var/lib/recon/ca/`.

`RECON_LLM_BASE_URL` and `RECON_LLM_MODEL` override `hub.yaml.llm.*`. If the
base URL is a private/link-local `http://` router IP, set
`RECON_LLM_ALLOW_INSECURE_HTTP=true`; public plaintext provider URLs remain
rejected to avoid bearer-token leakage.

Confirm effective non-secret config in the `resolved hub config` startup log:
`llm_model`, `llm_base_url_scheme`, `llm_base_url_host`,
`llm_allow_insecure_http`, `server_ip_san_source`, and
`server_ip_san_count`.

## 2. Reverse proxy (nginx)

If exposing the UI:

```bash
sudo install -m 0644 deploy/nginx/recon.conf /etc/nginx/sites-available/
sudo ln -s /etc/nginx/sites-available/recon.conf /etc/nginx/sites-enabled/
sudo certbot --nginx -d recon.example.com
sudo systemctl reload nginx
```

Open a browser, log in with the admin credentials. You should see an
empty Hosts page.

The shipped `recon.conf` already makes the live investigation channel work:
the `^/investigations/events/` location sets `proxy_buffering off`,
`proxy_read_timeout 6m`, `proxy_http_version 1.1`, and
`chunked_transfer_encoding off`. If you adapt the config, keep those — without
`proxy_buffering off` nginx withholds the SSE stream and investigation pages
look stuck on "Waiting for the model." (the hub also sends
`X-Accel-Buffering: no`). The approve-loop smoke check in §5 verifies the
stream actually flows through the proxy.

## 3. Issue a bootstrap token (per agent)

```bash
sudo -u recon /usr/local/bin/recon-hub \
  --config /etc/recon/hub.yaml \
  --mode gen-token \
  --agent-id <name-of-target-host>  \
  --token-ttl 1h
```

Copy the printed token — it is shown only once. Tokens are bound to a
single `agent_id` (security — see PROJECT.md §9.2).

Or use the **Settings** page in the UI: it generates the same token via a
form.

## 4. Agent install (on every target host)

```bash
sudo install -m 0755 recon-agent /usr/local/bin/
sudo install -d -m 0750 -o recon -g recon /etc/recon /var/lib/recon
```

Write `/etc/recon/agent.yaml`:

```yaml
hub:
  endpoint: "recon.example.com:9443"
  ca_cert: /var/lib/recon/hub-ca.pem
  cert:    /var/lib/recon/agent.pem
  key:     /var/lib/recon/agent.key
  bootstrap_token: /var/lib/recon/bootstrap.token
  server_name: recon.example.com

identity:
  id: <name-of-target-host>
  labels:
    env: prod
    role: app

runtime:
  max_concurrent_collectors: 4
  artifact_dir: /var/lib/recon/artifacts
  default_timeout: 30s
  heartbeat_interval: 15s
```

Drop the bootstrap token in:

```bash
echo '<token-from-step-3>' | sudo tee /var/lib/recon/bootstrap.token >/dev/null
sudo chmod 0600 /var/lib/recon/bootstrap.token
sudo chown recon:recon /var/lib/recon/bootstrap.token
```

Install sudoers (optional — for `journalctl`/`ss`/`iptables -L` collectors):

```bash
sudo install -m 0440 deploy/sudoers/recon /etc/sudoers.d/recon
```

Install systemd unit:

```bash
sudo install -m 0644 deploy/systemd/recon-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now recon-agent
sudo journalctl -u recon-agent -f
```

You should see `agent connected` in the hub log within 1–2 seconds.

## 5. Verify end-to-end

In the UI:

1. Open **/hosts** — your new agent shows `online`.
2. Click the agent → **Run** beside `system_info` → results in <100 ms.
3. **/investigations** → enter `find why nginx is restarting on app01`
   → step-by-step approve/skip the model's tool calls. After each approve the
   page advances on its own (no reload); if it sticks on "Waiting for the
   model.", verify nginx has `proxy_buffering off` on the SSE path so the live
   channel is not buffered.

## Common operations

- **Revoke an agent**: `recon-hub --mode revoke --agent-id host-X`. The
  current cert is rejected at next Connect; issue a fresh token to
  re-enroll.
- **Rotate operator password**: regenerate the hash, edit `/etc/recon/hub.env`,
  `systemctl restart recon-hub`.
- **Backup**: snapshot `/var/lib/recon/` (DB + CA + artifacts).
- **Cost cap**: set `llm.max_tokens_per_investigation` in `hub.yaml`. This is
  the spend budget, not the model context window; compaction triggers at 50%
  of the per-profile `context_window_tokens`.
- **Huge log arrays**: `llm.max_result_tokens` (env
  `RECON_LLM_MAX_RESULT_TOKENS`, default 2000) bounds the tokens per
  `collect` / `collect_batch` / `search_artifact` result. Fleet surveys over one
  log collector are rolled up across hosts and aged results are demoted to
  one-line re-read pointers (`history_keep_recent_results` /
  `history_demote_min_bytes`); full data stays behind `get_full_result` /
  `search_artifact`. The token estimate self-calibrates from the provider's
  reported `prompt_tokens`. Operator metrics: `cached_tokens` and
  `token_calibration_ratio` in `GET /api/v1/investigations/{id}`.
- **Prompt caching**: set `supports_prompt_cache: true` on a profile only when
  the backend honours Anthropic-style `cache_control` (e.g. OpenRouter →
  Anthropic); OpenAI caches the stable prefix automatically. Leave it off for
  OpenAI/vLLM. Effectiveness shows up as `cached_tokens`.
- **Multi-model routing**: add `llm.profiles` (summarizer/cheap/verifier
  tiers) to route cheaper models per operation. See the commented example in
  `deploy/docker/hub.yaml` and "Investigation memory & model routing" in the
  README. Notebooks live under `<artifact_dir>/investigations/<id>/notebook.md`
  and follow the same retention window as the investigation.

## Troubleshooting 502s

Check which component emitted the 502 before changing proxy timeouts:

- nginx 502s show up in nginx access/error logs for browser paths such as
  `/investigations/...` or `/investigations/events/...`.
- LLM router 502s show up in the hub journal as `investigator step` errors
  containing `llm chat: llm http 502` and the upstream response body.

For upstream errors such as `No tool call found for function call output`, the
hub filters orphaned tool-result messages before sending the next LLM request
and logs `dropped orphan tool results ...` with the investigation ID. If router
502s continue, inspect the external router at `RECON_LLM_BASE_URL` for the same
timestamp and verify `RECON_LLM_MODEL` is accepted by that router.

## Continuing aborted and completed investigations

Continuing an aborted **or completed (`done`)** investigation requires a live
LLM client. A completed investigation is reopened **in place** (status returns
to `active`, an `OPERATOR RESUME` turn is appended, and the prior conclusion is
preserved as a starting point). After changing
`RECON_LLM_API_KEY`, `RECON_LLM_BASE_URL`, or `RECON_LLM_MODEL`, restart the
hub and confirm startup logs. `LLM client ready` means continuation is enabled;
`LLM client disabled` includes non-secret fields such as `reason_class`,
`llm_model`, `llm_base_url_scheme`, and `llm_base_url_host`.

Expected UI states:

- `aborted` + LLM enabled -> `Continue investigation` form.
- `aborted` + LLM disabled -> recovery panel with LLM config keys and restart
  guidance.
- `done` + LLM enabled -> `Continue investigation` reopens the completed run in
  place (prior conclusion preserved). LLM disabled -> a panel explaining
  continuation needs a configured client and a hub restart.

A new investigation is also auto-seeded with a compact, host-scoped digest of
conclusions from prior `done` investigations (read-only "hints", bounded). Tune
it under `investigator.priors` in `hub.yaml`; set `investigator.priors.enabled:
false` to turn it off.

If a browser at `/investigations/continue` shows plaintext
`investigator disabled`, the running hub is stale or misconfigured. Restart the
updated hub and recheck the startup fields above.
