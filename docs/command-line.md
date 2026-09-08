# The command line

One binary, one command per job. Everything a command needs is a flag, and
every flag that a deployment sets once has an environment variable beside it.

```
charon serve      Accept inbound webhooks and record them
charon dispatch   Deliver recorded events to their destinations
charon route      Manage where a provider's events are delivered
charon user       Create an operator who can sign in to the panel
charon events     Act on recorded events
charon verify     Configure how a provider's signature is checked
charon sign       Configure how deliveries leaving here are signed
charon tenant     Manage the tenants events are recorded for
charon role       Manage roles and what they grant
charon retention  How long each tenant's events are kept
charon purge      Discard events past what their tenant keeps
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
| `-metrics-addr` | `CHARON_METRICS_ADDR` | — | address to serve Prometheus metrics on; off when empty |

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
| `-max-response-bytes` | `16384` | how much of what a destination says back is kept on the attempt |
| `-purge-every` | `1h` | how often to discard events past what their tenant keeps |

`dispatch` does not apply migrations — `serve` does — so on a rollout it can
come up first, against a schema the new build is ahead of. It waits for the
migrations this build carries before it starts delivering, and says so once. A
deployment where they never arrive is then one line in the log instead of an
error about a missing column on every round.

It does not poll on a fixed interval. A recorded event announces itself on
commit, so a new one is picked up at once, and between rounds the process sleeps
until the earliest retry is actually due. `-safety-interval` bounds that sleep,
because a delivery can become due through something that announced nothing — a
replay, a route enabled by hand, a lease that expired.

Retries are exponential with jitter, from `-backoff-base` up to `-backoff-cap`.
At `-max-attempts` the delivery is dead, which is never counted as delivered.

**Only what time can fix is retried.** A `5xx` or a broken connection says "not
now"; the next attempt might land. A `4xx` says the request itself is wrong,
and the request never changes — the same bytes go out every time, so twelve
attempts earn the same answer twelve times and the delivery dies hours later
than it could have. Those are settled on the first attempt.

Three exceptions are believed rather than argued with: `408 Request Timeout`,
`425 Too Early` and `429 Too Many Requests`.

A destination that answers with `Retry-After` is obeyed, in either form the
header allows, up to `-backoff-cap` — the receiver knows why it is busy and the
backoff curve does not, but no receiver gets to hold a delivery indefinitely.

A delivery settled this way is dead, not lost: the event is kept whole and
resend is how it goes again once the receiving side is fixed.

What a destination answers is kept on the attempt and shown on the event page.
A status code is usually the whole answer, and cannot be counted on to be: a
rejection carries a reason, a validation failure carries which field, a queue
carries the identifier it filed the thing under. A number on its own leaves
nobody able to act.

It is a diagnostic and not an archive, so `-max-response-bytes` bounds it and
what was cut says so on the page. Nothing about the answer is interpreted —
only the status decides whether a delivery is done.

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

An address is refused here as it is in the panel: it has to parse, have a host,
and be `http` or `https`. Nothing is said about the path — a trailing slash is a
real endpoint, and what belongs after the host is the receiver's business. A
destination of another kind of transport is addressed however that transport
addresses things, and this rule leaves it alone.
| `-tenant` | `default` | which tenant this route belongs to |

Correcting a destination's address tries what is waiting for it at once, with
its attempts back: what failed against an address that was wrong did not fail
against the destination, and a backoff earned by a typo is not worth serving
out. Saving without changing the address leaves the backoff alone, because a
destination that is genuinely down has earned it.

Switching a destination off pauses it. Nothing new is routed there, and what is
already waiting keeps waiting with its attempts intact rather than being spent
against somewhere deliberately taken out of service. Switching it back on
delivers both what was held and everything that arrived meanwhile.

A delivery that already reached the attempt limit is dead and stays dead; the
resend button on the routes page is how that comes back.

Two routes for one provider deliver the same event twice, once to each
destination. The same url twice for one provider is refused, because that is a
duplicate rather than a fan-out.

A provider name is never declared before it is used, so nothing can refuse one
that is misspelled — routing a provider before its first event is the normal
way to set one up. What `route add` does instead is say so, and name the
providers this tenant already knows, so a near miss is visible at once:

```
routed stipe to stipe (https://api.internal/webhooks/stripe)
warning: nothing has arrived for "stipe" and it has no verification configured; this tenant already knows github, stripe
```

## events

### force

Delivers events whose signature failed. Charon never does this on its own: a
proof that does not match is refused and stays refused, and this is an operator
saying, on the record, that this batch is genuine anyway.

```sh
charon events force -by you@example.com \
  -reason 'the sender rotated its token without telling us' \
  -id 01a0... -id 01a1...
```

| flag | purpose |
|---|---|
| `-by` | the operator taking the decision, by the address they sign in with |
| `-reason` | why, in at least ten characters; it is stored and shown for ever |
| `-id` | an event to deliver; give it once per event |
| `-tenant` | which tenant the events belong to |

**The verdict is not rewritten.** The event stays `invalid` or `missing`, and
gains a record of who let it through and why. What the signature said and what
an operator decided are different things and both stay readable — on the event
page, and on every attempt it produced.

One reason covers the whole batch, because somebody who has worked out why a
group of requests failed is explaining the group. Only a request that was
checked and refused can be overruled: there is nothing to overrule on one that
passed, or on one nothing was configured to check.

It needs `events.force`, which `admin` and `owner` hold and `operator` and
`viewer` do not. Passing a security check is not implied by being allowed to
send something again.

In the panel it is on the events list: tick what to send, say why, once.

## verify

How a provider's signature is checked. A provider with nothing configured is
accepted unchecked, exactly as before; once a verifier exists, a request whose
signature does not match is still recorded and marked, and never delivered.

```sh
export CHARON_SECRET_STRIPE='whsec_...'   # or a line in .env, under compose
charon verify set -provider stripe -preset stripe -secret-env CHARON_SECRET_STRIPE
charon verify set -provider whatsapp-bussines -preset whatsapp-bussines \
  -secret-env CHARON_SECRET_WHATSAPP -verify-token-env CHARON_VERIFY_WHATSAPP
charon verify presets
charon verify list
charon verify recheck -provider stripe
charon verify remove -provider stripe
```

| flag | purpose |
|---|---|
| `-provider` | whose requests are checked |
| `-secret-env` | **name** of the environment variable holding the secret |
| `-verify-token-env` | **name** of the environment variable holding the token a provider offers when it confirms the address, for one that will not accept an address until it answers |
| `-preset` | `stripe`, `charon`, `github`, `shopify`, `whatsapp-bussines`, `instagram`, `bearer-token`, `basic-auth` |
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

Configuring verification checks what is already recorded, on its own. A request
that arrived before any settings existed is `unchecked` — the absence of a
verdict rather than one — so the moment the settings arrive, the question
becomes askable and is asked. Nothing has to be run by hand for that.

`recheck` does the same on demand, for a provider, and is the way back from a
secret that was configured wrong: what now passes is reopened for delivery.

```sh
charon verify recheck -provider stripe
```

Neither touches a request that was already delivered. It keeps the answer it
went out with, because saying now that it was never signed would claim it had
been held back, and it was not.

Some providers confirm the address before sending anything: they `GET` it with
a token they were told and a value to echo. `-verify-token-env` names the
variable holding that token, and `GET /webhooks/{provider}` then answers `200`
with the echoed value, `403` to a wrong token, and `405` for a provider with no
handshake configured. A variable that is named but not set in the serving
process comes to the same `405`, which is what `verify list` reports as
`verify token from VAR (not set in this environment)`. The token and the
signing secret are independent: a provider can have either, both or neither.

## sign

How a delivery proves it came from Charon. Off until a destination has a
secret, because signing for a receiver that is not checking yet only breaks
deliveries.

```sh
charon sign generate
charon sign add -destination billing -secret env:CHARON_SIGNING_BILLING
charon sign add -destination billing -secret file:/run/secrets/billing
charon sign list
charon sign remove -destination billing -secret env:CHARON_SIGNING_BILLING
```

| flag | purpose |
|---|---|
| `-destination` | whose deliveries are signed |
| `-secret` | where the secret is: `env:NAME` or `file:/path`, never the secret |
| `-tenant` | which tenant the destination belongs to |

The database holds the reference, never the secret. `file:` is read on every
attempt, so a secret manager that rewrites the mounted file rotates it without
restarting anything; `env:` is read by **`charon dispatch`**, which is a
different process from `charon serve` and has a different environment.

A destination signs with every secret it has, which is what makes rotation
possible: both are live, the receiver accepts either, and the old one is
removed once it is no longer needed. `charon sign list` shows when each was
added.

`generate` prints a secret of the right shape and the lines to run with it. It
stores nothing.

The format, and how to verify it on the receiving side, are in
**[docs/signing.md](signing.md)**.

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
| `-role` | `viewer` | the role they hold there |
| `-superadmin` | off | make a system administrator instead |

How strong a password is belongs to whoever chooses it: the only thing refused
is none at all, because an account with no way in cannot be reached.

A **system administrator** belongs to no tenant and reaches every one, and is
the only account that creates and removes tenants. Only somebody who already is
one can make another, which is why the first is made here. Revoking asks for a
tenant and a role, because an account with neither reaches nothing.

Neither the account created here nor the one revoked back to a tenant gets more
than `viewer` unless `-role` says so. Standing is granted deliberately, never
by leaving a flag out.

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

A role shipped with Charon can be changed but not removed. Until it is changed
it grants what the running build says it grants, so a permission a new version
adds reaches it on the next start. Once changed it is the deployment's, and
starting leaves it alone — including when a later version would have widened
it.

## retention

How long a tenant's events are kept. Nothing is discarded until a tenant says
so: a database that grows is a smaller problem than one that quietly threw away
what somebody needed.

```sh
charon retention list
charon retention set -tenant acme -days 90
charon retention set -tenant acme -forever
```

| flag | purpose |
|---|---|
| `-tenant` | whose events these are |
| `-days` | how many days to keep, between 1 and 3650 |
| `-forever` | keep them all, which is where every tenant starts |

Discarding an event takes its request body, its deliveries and their attempts
with it, because none of them mean anything without it.

## purge

Applies what `retention` says. `charon dispatch` does this every hour on its
own, so this is for a deployment that would rather run it as a job, or for
seeing what would go.

```sh
charon purge -dry-run
charon purge
```

An event with a delivery still `pending` is never discarded, whatever its age.
A delivery that old means the destination has been unreachable for a long time,
which is a reason to look at it rather than to erase the evidence.

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

A secret is the same problem and `-e` is the wrong answer for it, because the
process that reads it is `charon` or `dispatcher`, not the throwaway container.
Both services read `.env` beside `docker-compose.yml`, so a secret goes there
and takes effect on the next `docker compose up -d`.
