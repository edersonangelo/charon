# Contributing to Charon

## Requirements

- Go 1.27 or newer
- Docker, for the local Postgres
- `make`

## Getting started

```sh
make hooks     # point git at .githooks (guards direct pushes)
make tools     # install golangci-lint into ./bin
make db-up     # start Postgres
make ci        # everything CI runs, in the same order
```

`make ci` is the contract. CI runs that exact target, so if it passes locally it
passes on the pull request. If the two ever diverge, that is a bug in the
Makefile, not in your change.

## Repository layout

```
cmd/charon/         the single binary and its subcommands
internal/ingest/    the inbound port: accept, store, acknowledge
internal/delivery/  the delivery loop: claim, attempt, back off, dead letter
internal/store/     SQL queries and the code sqlc generates from them
internal/provider/  per-provider signature verifiers and correlation extractors
internal/web/       operator panel templates and handlers
migrations/         plain .sql migration files, embedded into the binary
```

Everything lives under `internal/` until an external consumer justifies a public
API. Nothing is exported for the sake of it.

## Branching

`develop` is the trunk. Everything lands there, and `main` follows it at release
points.

- `develop` — the default branch, and the base for every pull request.
- `main` — production. Advances only to a released commit, which is tagged.
- `feature/<short-name>` — branched from `develop`, merged back by pull request.
- `hotfix/<short-name>` — branched from `main` when a release needs a fix.

Neither `develop` nor `main` is pushed to directly; the `pre-push` hook refuses.

## Pull requests

History is linear: pull requests are **squash-merged**, so a pull request
becomes exactly one commit on `develop`. There are no merge commits.

That has one consequence worth knowing before you start. The commits inside your
branch are discarded on merge — commit however you like while working, as often
and as messily as is useful. **The pull request title becomes the commit
message**, so that is the part that has to be right.

Titles follow [Conventional Commits](https://www.conventionalcommits.org), in
English:

```
<type>[(scope)][!]: <description>

type         build chore ci docs feat fix perf refactor revert style test
description  starts lowercase, at most 71 characters, no trailing period
!            marks a breaking change
```

```
feat(ingest): store raw payload before acknowledging
fix(delivery): stop counting dead-lettered events as delivered
refactor(store)!: replace the event key with a ULID
```

CI checks this. To check a title before opening the pull request:

```sh
scripts/check-pr-title.sh "feat(ingest): store raw payload before acknowledging"
```

## Generated code is committed

Output from `sqlc` and `templ` is checked in on purpose, so that
`go install github.com/edersonangelo/charon/cmd/charon@latest` works without
requiring either tool. Regenerate with `make generate` and commit the result in
the same change as the source it came from.

## Changing how something is built

A change that is hard to reverse — the schema, the HTTP surface, the deployment
artifact, a guarantee stated in the README — needs its reasoning in the pull
request: the context, what was decided, the alternatives, and what it costs.

If the alternatives cannot be argued convincingly, the decision is not ready.

## Testing

Correctness in Charon is defined by observable behaviour, not by implementation:

- Killing Postgres mid-request must not produce a `2xx`.
- A destination that hangs must leave the event pending, with the attempt
  recorded.
- A dead-lettered event must not appear in any delivered count.
- A replay must produce a new delivery and a new record, without mutating the
  original.

Prefer a test that asserts one of those over a test that asserts a function was
called. Run `make test-race` — the race detector is not optional here.
