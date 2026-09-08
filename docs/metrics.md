# Metrics

Charon reports what it is doing in the Prometheus exposition format. Nothing is
imported to produce it: it is a few lines of text, and a gateway does not need
a metrics library to print numbers it already has.

## Turning it on

```sh
charon serve -metrics-addr 127.0.0.1:9090
```

Off when the flag is absent, and on a **listener of its own** when it is. That
is deliberate. What a scrape reports is every tenant's counts at once, and the
main listener is the one the internet posts webhooks to — an operational
endpoint does not belong on a public port. Putting it behind the panel's
sign-in instead would only make it unscrapable.

Bind it where your collector can reach it and nobody else can: a private
interface, a sidecar, or a port your ingress does not publish.

```yaml
scrape_configs:
  - job_name: charon
    static_configs:
      - targets: ["charon:9090"]
```

## What it reports

| metric | type | labels |
|---|---|---|
| `charon_events_total` | gauge | `tenant`, `signature` |
| `charon_deliveries_total` | gauge | `tenant`, `state` |
| `charon_delivery_attempts_total` | counter | `tenant` |
| `charon_oldest_pending_seconds` | gauge | `tenant` |
| `charon_tenants` | gauge | |
| `charon_operators` | gauge | |
| `charon_build_info` | gauge | `version` |

`signature` is `valid`, `invalid`, `missing` or `unchecked`. `state` is
`pending`, `delivered` or `dead`.

A metric with nothing measured is left out rather than reported as zero, which
would be a claim nobody made.

## What to watch

**`charon_oldest_pending_seconds`** is the one number that says the gateway is
in trouble. It is how long the delivery that has been due longest has been
waiting, so it rises when a destination stops answering and falls on its own
when one starts again. Alert on it before alerting on anything else.

```
charon_oldest_pending_seconds > 300
```

**`charon_deliveries_total{state="dead"}`** only ever goes up, so alert on the
rate rather than the value:

```
increase(charon_deliveries_total{state="dead"}[15m]) > 0
```

A dead delivery reached its attempt limit and is never counted as delivered.
It does not come back on its own; somebody has to replay it.

**`charon_events_total{signature="invalid"}`** rising means a provider's
requests stopped verifying — usually a secret rotated at their end and not
here. Those events are recorded and never delivered, and
`charon verify recheck` is the way back once the secret is right.

## Health is separate

`GET /healthz` says the process is up and `GET /readyz` says the database is
reachable. Both are on the main listener, both are unauthenticated, and neither
reports a number about anybody's traffic — which is why they can be there and
metrics cannot.
