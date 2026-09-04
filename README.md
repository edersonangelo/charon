# Charon

An inbound webhook gateway. It accepts webhooks, stores them before
acknowledging, then routes and delivers them with retry, dead lettering, search
and replay.

Single binary, one PostgreSQL database, no other runtime dependencies.

## Status

Pre-alpha. Ingestion and delivery work: Charon accepts a webhook, records it
durably, acknowledges it only after that record is committed, then delivers it
to the destinations routed for its provider, retrying with backoff until the
destination answers `2xx` or the attempt limit is reached. There is no operator
panel and no signature verification yet. See [Roadmap](#roadmap).

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

To see what was recorded:

```sh
docker compose exec postgres psql -U charon -d charon \
  -c 'select provider, body_size, received_at from inbound_event'
```

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
charon migrate    Apply pending schema migrations and exit
charon version    Print the build version
```

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

`GET /healthz` reports that the process is up. `GET /readyz` reports whether the
database is reachable, which is the only thing that makes an instance worth
sending traffic to.

## Roadmap

| Phase | Scope | Status |
|---|---|---|
| 0 | Build, lint and test gate | done |
| 1 | Durable ingestion | done |
| 2 | Delivery loop: claim, retry, backoff, dead letter | done |
| 3 | Operator panel: search, inspect, replay | |
| 4 | Per-provider signature verification and correlation | |
| 5 | OIDC login, authenticated delivery, metrics | |
| 6 | `v0.1.0` release | |

## Documentation

- [CONTRIBUTING.md](CONTRIBUTING.md) — development setup, branching, commits

## Authorship

Some of the code in this repository was written with AI assistance.

The decisions were not. The architecture, the trade-offs and every change that
landed here were reviewed and decided by me, including the places where I
rejected what was proposed.

## License

[Apache-2.0](LICENSE)
