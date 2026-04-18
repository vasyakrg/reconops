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

## 1. Bootstrap

```bash
cp .env.example .env
```

Set `RECON_ADMIN_PASSWORD=strong-password` and `RECON_LLM_API_KEY=sk-or-v1-…`
in `.env`. The hub bcrypt-hashes the plaintext at startup — no separate
`gen-password-hash` step required.

If you'd rather hand out a pre-computed hash (CI, config management, etc.),
set `RECON_ADMIN_PASSWORD_HASH` instead — it wins over the plaintext when
both are set:

```bash
make compose-gen-hash PASSWORD='strong-password'
```

> **Production note:** edit `deploy/docker/hub.yaml` *before* the first start to
> add the hub's real DNS name(s) and IP(s) under `server.dns_names` /
> `server.ip_addrs`. The bootstrap CA bakes them into the server cert and
> changing them later means regenerating `/var/lib/recon/ca/`.
>
> Set `RECON_TLS_CN=<your hostname>` in `.env` so the nginx self-signed cert
> matches. Or bind-mount your real cert at `/etc/nginx/certs/server.{crt,key}`.

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
