# Charon

An inbound webhook gateway. It accepts webhooks, stores them before
acknowledging, then routes and delivers them with retry, dead lettering, search
and replay.

Single binary, one PostgreSQL database, no other runtime dependencies.

## Status

Pre-alpha. Ingestion works: Charon accepts a webhook, records it durably, and
acknowledges it only after that record is committed. Nothing is delivered
anywhere yet — that is the next phase. See [Roadmap](#roadmap).

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
charon serve     Accept inbound webhooks and record them
charon migrate   Apply pending schema migrations and exit
charon version   Print the build version
```

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
| 2 | Delivery loop: claim, retry, backoff, dead letter | |
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
