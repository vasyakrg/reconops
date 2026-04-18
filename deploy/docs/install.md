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

# Config + env file (env holds secrets only).
sudo install -m 0640 -o recon -g recon hub.yaml /etc/recon/hub.yaml
sudo install -m 0600 -o recon -g recon hub.env  /etc/recon/hub.env
```

`hub.env` minimal contents:

```
RECON_LLM_API_KEY=sk-or-v1-...
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
   → step-by-step approve/skip the model's tool calls.

## Common operations

- **Revoke an agent**: `recon-hub --mode revoke --agent-id host-X`. The
  current cert is rejected at next Connect; issue a fresh token to
  re-enroll.
- **Rotate operator password**: regenerate the hash, edit `/etc/recon/hub.env`,
  `systemctl restart recon-hub`.
- **Backup**: snapshot `/var/lib/recon/` (DB + CA + artifacts).
- **Cost cap**: set `llm.max_tokens_per_investigation` in `hub.yaml`.
