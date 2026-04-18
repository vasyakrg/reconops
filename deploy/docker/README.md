# Recon hub via docker compose

The compose stack runs **only the hub**. Agents are deployed on the target hosts
themselves (systemd unit in `deploy/systemd/recon-agent.service`); they connect
to the hub over mTLS on port `9443`.

## 1. Bootstrap

```bash
cp .env.example .env
```

Generate the bcrypt hash for the operator password and paste it into `.env`:

```bash
docker compose run --rm --no-deps \
  -e RECON_ADMIN_PASSWORD='strong-password' \
  --entrypoint /usr/local/bin/recon-hub \
  hub --mode gen-password-hash
```

Set `RECON_ADMIN_PASSWORD_HASH=<output>` and `RECON_LLM_API_KEY=sk-or-v1-…`
in `.env`.

> **Production note:** edit `deploy/docker/hub.yaml` *before* the first start to
> add the hub's real DNS name(s) and IP(s) under `server.dns_names` /
> `server.ip_addrs`. The bootstrap CA bakes them into the server cert and
> changing them later means regenerating `/var/lib/recon/ca/`.

## 2. Up

```bash
docker compose up -d
docker compose logs -f hub
```

The UI is on <http://localhost:8080> (front it with nginx + TLS in production —
see `deploy/nginx/recon.conf`). gRPC for agents on `localhost:9443`.

## 3. Issue a bootstrap token for an agent

Tokens are bound to a single `agent_id` and shown only once.

```bash
docker compose exec hub /usr/local/bin/recon-hub \
  --config /etc/recon/hub.yaml --mode gen-token \
  --agent-id prod-app-01 --token-ttl 24h
```

Copy the token, install the agent on the target host (`deploy/docs/install.md`),
write the token into `/var/lib/recon/bootstrap.token`, start `recon-agent`.

## 4. Revoke an agent

```bash
docker compose exec hub /usr/local/bin/recon-hub \
  --config /etc/recon/hub.yaml --mode revoke \
  --agent-id prod-app-01 --revoke-reason "host decommissioned"
```

The next `Connect` is rejected. To re-enrol, issue a fresh bootstrap token.

## 5. Backups & state

Everything that matters lives in the `recon-state` named volume:

* `recon.db` — investigations, hosts, tool calls, findings, audit log
* `artifacts/` — collector outputs (large, retention-trimmed at 7 days)
* `ca/` — bootstrap CA + server cert (re-generating these invalidates every
  enrolled agent — handle with care)

Snapshot:

```bash
docker run --rm -v recon-state:/var/lib/recon -v "$PWD":/backup alpine \
  tar czf /backup/recon-state-$(date +%F).tar.gz -C /var/lib/recon .
```

Restore: `tar xzf` into a fresh empty volume before bringing the stack up.

## 6. Upgrade

```bash
git pull
docker compose build --pull hub
docker compose up -d hub
```

The hub re-applies SQLite migrations on start; the volume's data carries over.
