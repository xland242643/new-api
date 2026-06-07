# Public New API Gateway Deployment

This deployment reuses the existing PostgreSQL, Redis, Nginx Proxy Manager,
`shared_web`, and `shared_internal` infrastructure documented for
`metalearn.top`. It does not expose New API port `3000` on the host.

## Recommended Layout

```text
/srv/server/
├── infrastructure/
└── new-api/
    ├── docker-compose.yml
    └── .env
```

Keep the existing `ai.metalearn.top` domain for Open WebUI. Use the dedicated
domain `gateway.metalearn.top` for New API. New API owns many `/api/*` routes,
so putting it under the existing `api.metalearn.top` path router would create
route conflicts.

The services coexist as follows:

```text
ai.metalearn.top
  -> Nginx Proxy Manager
  -> open-webui:8080
  -> http://new-api:3000/v1 (shared_web Docker network)

gateway.metalearn.top
  -> Nginx Proxy Manager
  -> new-api:3000
  -> DeepSeek and other upstream model providers
```

New API is an independently operated public API relay. Open WebUI and MetaLearn
are two clients of that relay:

- Public relay users register on `gateway.metalearn.top`, top up, and create
  their own API tokens.
- Open WebUI is an optional browser chat frontend backed by one restricted
  New API service token.
- MetaLearn is an app integration. Its backend calls New API over the internal
  Docker network and assigns usage to the `metalearn-app` group.

Do not ship a New API administrator token or shared service token in the
MetaLearn app.

## Product And Account Boundaries

Use separate New API groups so each product surface can have independent model
access, rates, and limits:

| Group | Users | Recommended access |
|---|---|---|
| `public` | Public API relay customers | Supported public models with retail rates |
| `open-webui` | Shared browser chat frontend | Curated chat/image models and a hard quota |
| `metalearn-app` | MetaLearn app shadow users | Only models required by the app |
| `internal` | Operator testing | Restricted access, never distributed |

For the first release, keep the wallets separate:

- Public relay balance is purchased and consumed on New API.
- MetaLearn app credits are purchased through StoreKit and granted through the
  MetaLearn backend.

Sharing one wallet between the public relay and the iOS app requires account
linking, StoreKit receipt handling, refunds, and cross-product reconciliation.
Add that only after both products have stable usage.

## 1. Check Capacity

```bash
free -h
docker stats --no-stream
docker network inspect shared_web >/dev/null
docker network inspect shared_internal >/dev/null
```

Keep the existing 2 GB swap enabled. The Compose file caps New API at 384 MB.
If available RAM regularly falls below 300 MB, upgrade the server before
adding more model gateways or workers.

The current 2 vCPU / 2 GB server is suitable for setup and a small private
beta. Before advertising a public relay, upgrade to at least 2 vCPU / 4 GB RAM
or move Open WebUI to another instance. Swap prevents abrupt OOM kills but does
not provide acceptable performance under public traffic.

## 2. Create A Dedicated Database User And Database

Generate a URL-safe hex password:

```bash
openssl rand -hex 24
```

Open PostgreSQL:

```bash
docker exec -it postgres psql -U postgres
```

Run the following SQL, replacing the password:

```sql
CREATE USER new_api WITH PASSWORD 'REPLACE_DB_PASSWORD';
CREATE DATABASE new_api OWNER new_api;
\q
```

## 3. Install The Deployment Files

Create the server directory and place `docker-compose.yml` and `.env` there:

```bash
mkdir -p /srv/server/new-api
cd /srv/server/new-api
```

Generate the remaining secrets:

```bash
openssl rand -hex 32
openssl rand -hex 32
```

Create `.env` from `server.env.example`, replace all placeholders, then protect
it:

```bash
chmod 600 .env
```

Do not commit `.env` or paste `docker compose config` output into logs because
it expands all secrets.

## 4. Start And Verify

```bash
cd /srv/server/new-api
docker compose config --quiet
docker compose pull
docker compose up -d
docker compose ps
docker compose logs --tail=100 new-api
docker exec new-api wget -qO- http://localhost:3000/api/status
```

The first administrator is created from New API's setup page. Create it before
exposing the service broadly and use a unique password. For a public relay,
enable registration only after email verification, abuse controls, pricing,
payment, user agreement, and privacy policy are configured.

## 5. Configure DNS And Nginx Proxy Manager

Add an Alibaba Cloud DNS record:

| Type | Host | Value |
|---|---|---|
| A | `gateway` | `47.106.8.43` |

Create an Nginx Proxy Manager Proxy Host:

| Setting | Value |
|---|---|
| Domain Names | `gateway.metalearn.top` |
| Scheme | `http` |
| Forward Hostname | `new-api` |
| Forward Port | `3000` |
| Block Common Exploits | enabled |
| Websockets Support | enabled |
| SSL | new Let's Encrypt certificate |
| Force SSL | enabled |

Do not open port `3000` in UFW or the Alibaba Cloud security group.

Verify externally:

```bash
curl -fsS https://gateway.metalearn.top/api/status
```

## 6. Connect The Existing Open WebUI

Keep the existing Nginx Proxy Manager host unchanged:

```text
ai.metalearn.top -> open-webui:8080
```

Because both containers join `shared_web`, configure the OpenAI-compatible
connection in Open WebUI as:

```text
URL: http://new-api:3000/v1
API Key: a restricted New API token created for Open WebUI
```

Do not use `https://gateway.metalearn.top/v1` from Open WebUI unless internal
Docker DNS is unavailable. The internal URL avoids an unnecessary public
network round trip.

The shared Open WebUI token measures all Open WebUI traffic together. It is
appropriate for a shared chat benefit, but not for charging individual Open
WebUI users. If individual Open WebUI billing is needed, require users to enter
their own public relay tokens.

Choose one Open WebUI policy:

- Private benefit: disable Open WebUI public signup and keep a hard quota on
  its shared token.
- Public BYOK chat: allow signup, but require each user to enter a token issued
  from `gateway.metalearn.top`.

Do not leave Open WebUI public signup enabled with an unrestricted shared token.

## 7. Configure The Public Relay

Before opening registration:

1. Add only upstream channels that permit your intended use and resale.
2. Configure model ratios, completion ratios, retail margin, and user groups.
3. Set default user quota to a small trial amount or zero.
4. Configure email verification, Turnstile, rate limits, and failed-request
   retry limits.
5. Configure public pricing, user agreement, privacy policy, support contact,
   and service notices.
6. Configure supported web top-up providers or issue redemption codes.
7. Limit expensive models and set per-user/token quotas.
8. Enable administrator 2FA and keep administrator credentials out of apps.
9. Back up PostgreSQL and monitor upstream spending every day.

New API already provides public registration, user balances, top-up records,
redemption codes, model ratios, user groups, token quotas, and usage logs.

Operating a public generative AI relay or API resale service can require
upstream authorization, filing, content-safety controls, real-name processes,
log retention, tax, and payment compliance. Confirm those obligations before
opening it to the public.

## 8. Configure New API For MetaLearn

1. Add the DeepSeek channel using the upstream DeepSeek API key.
2. Expose only `deepseek-v4-flash` and `deepseek-v4-pro` initially.
3. Create the `metalearn-app` group with app-specific model and pricing rules.
4. The MetaLearn backend creates or maps one New API shadow user/token per app
   user and stores that mapping server-side.
5. The app calls the MetaLearn backend; the backend calls
   `http://new-api:3000/v1` with the mapped restricted token.
6. Set token quota and model restrictions; never ship an administrator or
   shared service token.
7. Query New API usage from the backend and return app-friendly AI points to
   the client.

New API should meter and enforce model usage. A small MetaLearn backend should
still verify StoreKit purchases, maintain an idempotent purchase ledger, and
grant or revoke the corresponding New API quota.

```text
MetaLearn app
  -> api.metalearn.top/api/v1/ai/*
  -> MetaLearn backend
  -> http://new-api:3000/v1
  -> upstream model
```

## Operations

```bash
# Update
cd /srv/server/new-api
docker compose pull
docker compose up -d

# Logs and resource use
docker compose logs -f --tail=100 new-api
docker stats new-api

# Stop without deleting data
docker compose down
```

Back up the `new_api` PostgreSQL database before upgrades:

```bash
docker exec postgres pg_dump -U postgres -Fc new_api > new_api.dump
```
