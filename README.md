# Charon

An inbound webhook gateway. It accepts webhooks, stores them before
acknowledging, then routes and delivers them with retry, dead lettering, search
and replay.

Single binary, one PostgreSQL database, no other runtime dependencies.

## Status

Pre-alpha — not usable yet. What exists today is the build, lint and test gate.
The sections below fill in as each phase lands; see [Roadmap](#roadmap).

## Requirements

- PostgreSQL — the development setup uses 18
- Go 1.27 or newer, to build from source

## Install

```sh
git clone https://github.com/edersonangelo/charon.git
cd charon
make build
```

Binary releases, a container image and `go install` support are published
starting at `v0.1.0`.

## Usage

```sh
./bin/charon version   # print the build version
./bin/charon help      # list available commands
```

No serving commands are implemented yet.

## Roadmap

| Phase | Scope | Status |
|---|---|---|
| 0 | Build, lint and test gate | in progress |
| 1 | Durable ingestion | |
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
