<p align="center">
  <img src="docs/charon.png" alt="" width="390"/>
</p>

<h1 align="center">Charon</h1>

<p align="center">
  An inbound webhook gateway. It accepts webhooks, stores them before
  acknowledging, then routes and delivers them with retry, dead lettering,
  search and replay.
</p>

<p align="center">
  Single binary, one PostgreSQL database, no other runtime dependencies.
</p>

## Status

Pre-alpha. Ingestion, delivery, signature verification and the operator panel
work: Charon accepts a webhook, checks its signature, records it durably,
acknowledges it only after that record is committed, then delivers it to the
destinations routed for its provider, retrying with backoff until the
destination answers `2xx` or the attempt limit is reached. Every event and
every attempt is searchable and replayable from the panel. See
[Roadmap](#roadmap).

## Requirements

- Docker, to run the stack and the tests
- PostgreSQL 18 if you run it yourself; Go 1.27 or newer to build from source

## Quick start

```sh
git clone https://github.com/edersonangelo/charon.git
cd charon
docker compose up -d
```

That is the whole thing: PostgreSQL, the schema, and Charon listening on
`:8080`. Then send it something:

```sh
curl -i -X POST localhost:8080/webhooks/stripe \
  -H 'Content-Type: application/json' \
  -d '{"id":"evt_1","type":"charge.succeeded"}'
```

```
HTTP/1.1 202 Accepted
Content-Type: application/json

{"id":"01a06d07-dbe6-76fc-b20d-cb1a70e16c12"}
```

`202` means the request is committed to the database, not that anything has
been done with it. The identifier is how the event is found again.

Malformed bodies, unknown providers and unmapped event types are accepted and
recorded too. The inbound port does not read the payload, so nothing about its
content can cause a rejection.

Create an operator and open the panel at <http://localhost:8080>:

```sh
docker compose run --rm charon user add -email you@example.com
```

That operator owns the `default` tenant, which is the only one until you add
another.

A [Postman collection](contrib/postman) covers every route and asserts its own
status codes, so a green run is a working gateway.

PostgreSQL is published on `5434` and Charon on `8080`. Override with
`POSTGRES_HOST_PORT` and `CHARON_HOST_PORT` if either is taken.

## Running from source

```sh
docker compose up -d postgres
make build

export CHARON_DATABASE_URL='postgres://charon:charon@localhost:5434/charon?sslmode=disable'
./bin/charon serve
```

Binary releases, a published container image and `go install` support arrive at
`v0.1.0`.

## Commands

```
charon serve      Accept inbound webhooks and record them
charon dispatch   Deliver recorded events to their destinations
charon route      Manage where a provider's events are delivered
charon user       Create an operator who can sign in to the panel
charon verify     Configure how a provider's signature is checked
charon tenant     Manage the tenants events are recorded for
charon role       Manage roles, what they grant and who gets them
charon migrate    Apply pending schema migrations and exit
charon version    Print the build version
```

Every flag, with the reason it exists, is in
[docs/command-line.md](docs/command-line.md).

## Verification

A provider's signature is checked from stored parameters, not from code written
for that provider. Presets cover the common ones:

```sh
export CHARON_SECRET_STRIPE='whsec_...'
docker compose run --rm charon verify set -provider stripe \
  -preset stripe -secret-env CHARON_SECRET_STRIPE
docker compose run --rm charon verify presets
docker compose run --rm charon verify list
```

The database holds the name of the environment variable, never the secret. A
provider that no preset fits is configured by its parameters instead —
`-verifier`, `-scheme`, `-algorithm`, `-encoding`, `-header` — which is the
same mechanism the presets are made of.

A signature that fails does not reject the request: the outcome is recorded on
the event and shown in the panel, and a request that failed is kept so it can
be looked at. Nothing that failed is delivered. Once the secret is right,
`charon verify recheck -provider stripe` checks the refused requests again and
reopens the ones that now pass.

## Tenants

Everything recorded belongs to a tenant: events, routes, destinations,
verification settings and operators. A deployment that never asks for a second
one works as if the concept did not exist — `default` is created by the
migration and every command acts for it.

```sh
docker compose run --rm charon tenant add -slug acme -name 'Acme Inc'
docker compose run --rm charon tenant list
```

The panel has a tenants page for the same thing, for an operator whose role
grants `tenants.write`. An operator belongs to one tenant; a **system
administrator** belongs to none and moves between them, which is the only way
to reach more than one. The first one is made with
`charon user superadmin -email you@example.com`, and only somebody who already
is one can make another.

The address a webhook arrives at decides the tenant:

```
POST /webhooks/{provider}            the default tenant
POST /webhooks/{tenant}/{provider}   that tenant
```

An address naming a tenant that does not exist is refused with `404`, and it is
the only rejection Charon makes that is not about a resource limit. Every other
command takes `-tenant`, which defaults to `default` and to `CHARON_TENANT`.

Keeping tenants apart is Charon's job: every row carries the tenant it belongs
to, and every query is scoped to one. Nothing else is required of the
deployment — one database, one role, and creating a tenant is creating a
tenant.

The same rule exists one layer down for a deployment that wants it. Every
tenant scoped table has row level security forced on it, and the tenant a
connection acts for is set on the connection, so the database refuses another
tenant's rows whatever a query asks for. PostgreSQL exempts superusers and
roles carrying `BYPASSRLS`, so that layer applies only when `charon serve`
connects as a role with neither:

```sql
create role charon_app login password '<choose one>' nosuperuser nobypassrls;
grant usage on schema public to charon_app;
grant select, insert, update, delete on all tables in schema public to charon_app;
grant execute on all functions in schema public to charon_app;
```

It is worth having and it is not a prerequisite. `charon dispatch` acts for no
tenant — it delivers rows that each carry their own — so it stays on a role
that is not confined.

## Routing

Nothing is delivered until a provider has a route. Adding one takes no deploy
and no restart:

```sh
docker compose run --rm charon route add -provider stripe -url https://api.internal/webhooks/stripe
docker compose run --rm charon route list
```

An event recorded for a provider with no enabled route waits. It is not
discarded and it is not marked as handled: the moment a route appears, the next
dispatch round delivers it. Configuring the route after the first event arrives
costs nothing.

## Delivery

`charon dispatch` claims due deliveries under a lease, posts the raw body to the
destination, and closes the delivery only when the destination answers `2xx`.
Anything else counts an attempt and schedules a retry with exponential backoff
and jitter. At the attempt limit the delivery becomes `dead`, which is never
counted as delivered.

Each request carries `X-Charon-Delivery-Id`, `X-Charon-Event-Id`,
`X-Charon-Provider` and `X-Charon-Attempt`, plus the original `Content-Type`.

It does not poll on a fixed interval. An inbound record announces itself on
commit, so a new event is picked up immediately, and between rounds the
dispatcher sleeps until the earliest retry is actually due. The safety interval
bounds that sleep, because a delivery can become due through a route that
announced nothing — a replay, a manual change, an expired lease.

| Flag | Default | Purpose |
|---|---|---|
| `-workers` | `8` | Deliveries attempted at once |
| `-batch-size` | `50` | Deliveries claimed per round |
| `-safety-interval` | `30s` | Longest sleep before scanning anyway |
| `-request-timeout` | `15s` | How long a destination has to answer |
| `-max-attempts` | `12` | Attempts before dead lettering |
| `-backoff-base` | `5s` | First retry window |
| `-backoff-cap` | `1h` | Largest retry window |

## Operator panel

The panel is served by the same binary, on the same port, with no separate
build step and no external assets. Sign in with an operator created by
`charon user add`.

- **Search** by provider, delivery state, time window, event identifier, or any
  text inside the recorded body.
- **Inspect** an event by clicking anywhere on its row, which opens the detail
  in a dialog: its headers and raw body exactly as received, every delivery it
  produced, and the full attempt history of each one — when it was tried, what
  the destination answered, how long it took, and why it failed. Each row also
  links to the same detail as its own page.
- **Configure** everything from one place. The panel has two entries: events,
  which is the operation, and settings, which is routes, verification,
  operators, roles and tenants. Every control obeys the role of whoever is
  looking, on the page and at the route behind it: a control an operator may not
  use is not drawn, and the route behind it refuses anyway.
- **Replay** a single delivery or every delivery of an event. A replay resets
  the delivery to pending and announces it, so it is picked up immediately. It
  does not erase the attempt history, and it unplans the event so a route added
  since is included.

Replaying is a real redelivery, not a simulation. The destination will receive
the event again, which is the same situation described below.

## Roles

What an operator may do is a role, and a role is a set of permissions. The
permissions are fixed, because each one names something the code checks; the
roles are not, because which of them a deployment wants is its own business.
Four are shipped. They belong to no tenant and can be held in any, and a tenant
may define roles of its own alongside them:

| Role | Grants |
|---|---|
| `viewer` | read events, routes, verification and operators |
| `operator` | everything a viewer can, and replay events |
| `admin` | everything an operator can, and configure the tenant |
| `owner` | everything an admin can, and manage tenants |

Somebody belongs to a tenant as a role, and can belong to several holding a
different one in each — a viewer here, an owner there. They see one tenant at a
time, chosen in the panel's header. Belonging without a role stated is
belonging as a viewer, which is the least there is.

All of it is done from the panel, on the operators page, and the command line
mirrors it:

```sh
docker compose run --rm charon role permissions
docker compose run --rm charon role list
docker compose run --rm charon role set -name resender \
  -description 'sends events again' -grant events.read,events.replay
```

A permission nothing enforces cannot be granted. The set is closed in the code,
because each permission names something a route checks, and it is a table in
the database, so a grant points at a row: naming one that does not exist fails
on a foreign key, not only on a check that could be skipped. The panel obeys
the role on both sides — a control an operator may not use is not drawn, and
the route behind it refuses anyway.

Somebody arriving from single sign-on holds the role the provider names —
`/acme/operator` — or the least when it names only the tenant, and the panel
decides from there. See [Single sign-on](#single-sign-on).

## Single sign-on

The panel accepts any OpenID Connect provider, discovered from its issuer url.
Local accounts keep working alongside it, and nothing about single sign-on is
gated: no licence key, no seat count, no call to any service this project
controls.

```sh
CHARON_OIDC_ISSUER=https://sso.example.com/realms/main
CHARON_OIDC_CLIENT_ID=charon
CHARON_OIDC_CLIENT_SECRET=...
CHARON_OIDC_REDIRECT_URL=https://charon.example.com/auth/oidc/callback
```

The flow is authorization code with PKCE. The identity token is verified
against the provider's keys and the nonce is checked against the one issued.

Where somebody lands is read from the values the provider sends — its groups,
its roles, whatever it calls them. A value is a path: `/acme/operator` puts them
in the tenant `acme` as an operator, `/acme` puts them there at the least, and a
value naming no tenant is pointed at one from the panel. The token is what says
where somebody belongs, so it is settled at every sign-in: a value that stops
arriving takes the tenant with it.

Every provider needs one switch turned on for any of that to arrive, because
OpenID Connect standardises identity and not authorisation.
**[docs/single-sign-on.md](docs/single-sign-on.md)** is that switch, per
provider, with a worked Keycloak example and what the log says when it is
missing.

## Delivery is at-least-once

Charon guarantees a recorded event reaches its destination. It does not
guarantee it reaches it exactly once, and it cannot.

A destination can accept a delivery and have the acknowledgement lost on the way
back: a dropped connection, a timeout on Charon's side, a restart between the
`2xx` and the write that records it. None of those look any different from a
destination that received nothing, so Charon retries, and the handler sees the
same event again.

Duplicates also arrive without Charon's help. Providers retry on their own, and
two providers can report the same fact through independent paths.

**Consumers have to be idempotent.** The question to answer is not "have I seen
these bytes before" but "have I already applied this effect", against your own
state and keyed by the provider's event identifier. Recording a status can be
repeated safely; charging a card cannot, and only the consumer knows which one it
is doing.

## Configuration

| Flag | Environment | Default | Purpose |
|---|---|---|---|
| `-database-url` | `CHARON_DATABASE_URL` | — | PostgreSQL connection string. Required |
| `-addr` | `CHARON_ADDR` | `:8080` | Address to listen on |
| `-log-level` | `CHARON_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `-max-body-bytes` | — | `1048576` | Largest request body that will be recorded |
| `-migrate` | — | `true` | Apply pending migrations before serving |
| `-session-ttl` | — | `12h` | How long a panel session lasts |
| `-secure-cookie` | — | off | Mark the session cookie secure, for serving over HTTPS |
| `-verification-refresh` | — | `1m` | How often verification settings are reloaded regardless of announcements |
| `-tenant` | `CHARON_TENANT` | `default` | Tenant the command acts for, on every command but `serve` and `dispatch` |

`GET /healthz` reports that the process is up. `GET /readyz` reports whether the
database is reachable, which is the only thing that makes an instance worth
sending traffic to.

## Roadmap

| Phase | Scope | Status |
|---|---|---|
| 0 | Build, lint and test gate | done |
| 1 | Durable ingestion | done |
| 2 | Delivery loop: claim, retry, backoff, dead letter | done |
| 3 | Operator panel: search, inspect, replay | done |
| 4 | Signature verification | done |
| 5 | Multiple tenants, configurable roles, administration in the panel | done |
| 6 | Retention and purge, metrics | |
| 7 | `v0.1.0` release | |

## Documentation

- [docs/command-line.md](docs/command-line.md) — every command and flag, and why each one is there
- [docs/single-sign-on.md](docs/single-sign-on.md) — connecting an identity provider, and how it decides who lands where
- [CONTRIBUTING.md](CONTRIBUTING.md) — development setup, branching, commits

## Authorship

Some of the code in this repository was written with AI assistance.

The decisions were not. The architecture, the trade-offs and every change that
landed here were reviewed and decided by me, including the places where I
rejected what was proposed.

## License

[Apache-2.0](LICENSE)
