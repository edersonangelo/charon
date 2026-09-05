# The command line

One binary, one command per job. Everything a command needs is a flag, and
every flag that a deployment sets once has an environment variable beside it.

```
charon serve      Accept inbound webhooks and record them
charon dispatch   Deliver recorded events to their destinations
charon route      Manage where a provider's events are delivered
charon user       Create an operator who can sign in to the panel
charon verify     Configure how a provider's signature is checked
charon tenant     Manage the tenants events are recorded for
charon role       Manage roles and what they grant
charon migrate    Apply pending schema migrations and exit
charon version    Print the build version and exit
```

`charon <command> -h` prints the flags that command accepts. This page is the
same thing with the reasons attached.

## What every command needs

**`-database-url`**, or `CHARON_DATABASE_URL`. There is no default: a
connection string is the one thing Charon cannot guess.

```sh
export CHARON_DATABASE_URL='postgres://charon:charon@localhost:5434/charon?sslmode=disable'
```

**`-tenant`**, or `CHARON_TENANT`, on every command that touches what belongs to
somebody: `route`, `verify`, `role`, and `user add`. It defaults to `default`,
the tenant every deployment has, so a deployment that never wants a second one
never types it.

```sh
charon route list                    # the default tenant
charon route list -tenant acme       # another one
```

## serve

Accepts webhooks, records them, and serves the panel. One process does both
because they share the same database and the same lifetime.

| flag | environment | default | purpose |
|---|---|---|---|
| `-addr` | `CHARON_ADDR` | `:8080` | address to listen on |
| `-database-url` | `CHARON_DATABASE_URL` | — | required |
| `-log-level` | `CHARON_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `-max-body-bytes` | — | `1048576` | largest body that will be recorded; anything bigger is refused with 413 |
| `-migrate` | — | `true` | apply pending migrations before serving |
| `-session-ttl` | — | `12h` | how long an operator stays signed in |
| `-secure-cookie` | — | off | mark the session cookie Secure; turn on behind HTTPS |
| `-verification-refresh` | — | `1m` | longest the signature settings can be stale before being reloaded anyway |

The single sign-on flags live in
**[docs/single-sign-on.md](single-sign-on.md)**, along with what a provider has
to be told.

`-max-body-bytes` is a resource limit and not content validation: a body that
cannot be held cannot be recorded, and 413 is the only honest answer. Nothing
else about a payload can cause a rejection.

## dispatch

Delivers what `serve` recorded. A separate process because delivering is
outbound work with its own failure modes, and because it can be scaled or
stopped without touching the port that receives.

| flag | default | purpose |
|---|---|---|
| `-workers` | `8` | deliveries attempted at once |
| `-batch-size` | `50` | deliveries claimed per round |
| `-safety-interval` | `30s` | longest sleep before scanning anyway |
| `-request-timeout` | `15s` | how long a destination has to answer |
| `-max-attempts` | `12` | attempts before a delivery is dead lettered |
| `-backoff-base` | `5s` | first retry window |
| `-backoff-cap` | `1h` | largest retry window |

It does not poll on a fixed interval. A recorded event announces itself on
commit, so a new one is picked up at once, and between rounds the process sleeps
until the earliest retry is actually due. `-safety-interval` bounds that sleep,
because a delivery can become due through something that announced nothing — a
replay, a route enabled by hand, a lease that expired.

Retries are exponential with jitter, from `-backoff-base` up to `-backoff-cap`.
At `-max-attempts` the delivery is dead, which is never counted as delivered.

## route

Where a provider's events are delivered. Nothing is delivered until a provider
has a route, and an event recorded before the route existed is picked up the
moment it appears.

```sh
charon route add -provider stripe -url https://api.internal/webhooks/stripe
charon route add -provider stripe -url https://audit.internal/stripe -destination audit
charon route list
```

| flag | default | purpose |
|---|---|---|
| `-provider` | — | the provider whose events are routed |
| `-url` | — | where they are delivered |
| `-destination` | the provider's name | a name for this destination |
| `-transport` | `http` | kind of destination |
| `-tenant` | `default` | which tenant this route belongs to |

Two routes for one provider deliver the same event twice, once to each
destination. The same url twice for one provider is refused, because that is a
duplicate rather than a fan-out.

## verify

How a provider's signature is checked. A provider with nothing configured is
accepted unchecked, exactly as before; once a verifier exists, a request whose
signature does not match is still recorded and marked, and never delivered.

```sh
export CHARON_SECRET_STRIPE='whsec_...'
charon verify set -provider stripe -preset stripe -secret-env CHARON_SECRET_STRIPE
charon verify presets
charon verify list
charon verify recheck -provider stripe
charon verify remove -provider stripe
```

| flag | purpose |
|---|---|
| `-provider` | whose requests are checked |
| `-secret-env` | **name** of the environment variable holding the secret |
| `-preset` | `stripe`, `github`, `shopify`, `bearer-token`, `basic-auth` |
| `-verifier` | `hmac`, `shared-token`, `basic-auth` |
| `-scheme` | `simple`, `advanced` |
| `-algorithm` | `sha1`, `sha256`, `sha512` |
| `-encoding` | `hex`, `base64` |
| `-header` | header carrying the proof |
| `-timestamp-key` | key of the timestamp inside an advanced header |
| `-signature-key` | key of the digests inside an advanced header |
| `-tolerance` | how far a signed timestamp may be from now |

The database holds the **name** of the environment variable, never the secret,
so a dump of it cannot be used to forge a signature.

A preset supplies the parameters and anything given explicitly wins over it, so
a provider that is almost Stripe is a preset and one flag. Settings that cannot
build are refused here rather than silently refusing every request later.

`recheck` runs verification again over the requests of a provider that were
recorded invalid or missing, and reopens the ones that now pass. It is the way
back from a secret that was configured wrong.

## tenant

The tenants events are recorded for. A deployment that never asks for a second
one works as if the concept did not exist.

```sh
charon tenant add -slug acme -name 'Acme Inc'
charon tenant list
charon tenant remove -slug acme
```

The slug is what appears in the webhook address —
`POST /webhooks/acme/{provider}` — and what an identity provider's groups are
matched against. Removing a tenant removes everything recorded for it.

## user

Operators who sign in to the panel.

```sh
charon user add -email you@example.com
charon user add -email them@example.com -tenant acme -role operator
charon user superadmin -email you@example.com
charon user superadmin -email them@example.com -revoke -tenant acme -role admin
```

| flag | default | purpose |
|---|---|---|
| `-email` | — | the address that signs in |
| `-password` | prompted for | `CHARON_PASSWORD` when scripting |
| `-tenant` | `default` | which tenant they belong to |
| `-role` | `owner` | the role they hold there |
| `-superadmin` | off | make a system administrator instead |

How strong a password is belongs to whoever chooses it: the only thing refused
is none at all, because an account with no way in cannot be reached.

A **system administrator** belongs to no tenant and reaches every one, and is
the only account that creates and removes tenants. Only somebody who already is
one can make another, which is why the first is made here. Revoking asks for a
tenant and a role, because an account with neither reaches nothing.

## role

What an operator may do. The four Charon ships — `viewer`, `operator`, `admin`,
`owner` — belong to no tenant and can be held in any; a tenant may define roles
of its own alongside them.

```sh
charon role permissions
charon role list
charon role set -name resender -description 'sends events again' \
  -grant events.read,events.replay
charon role remove -name resender
```

A permission nothing enforces cannot be granted: each one names something a
route in the panel checks, so the set is closed in the code and held as rows in
the database. `charon role permissions` prints it with what each one allows.

A role shipped with Charon can be changed but not removed.

## migrate

Applies pending migrations and exits. `serve` does it too unless
`-migrate=false`, so this is for a deployment that would rather migrate as a
step of its own — a job before the rollout, for instance.

```sh
charon migrate
```

Migrations take an advisory lock, so two processes starting at once is not a
problem. Applying them twice is not either: what is already applied is skipped.

## Running commands against the compose stack

```sh
docker compose run --rm charon tenant list
docker compose run --rm -e CHARON_PASSWORD='...' charon user add -email you@example.com
```

`docker compose run` starts a throwaway container with the same environment as
the service, which is why it needs no `-database-url`. Note that `-e` is
required to pass a variable in: the shell's environment is not the container's.
