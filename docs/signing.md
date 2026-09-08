# Signing what leaves here

Charon checks the signature a provider sends. Until a destination is signed,
it does not offer the same to whoever it delivers to: a service behind Charon
that used to verify Stripe's signature itself can verify nothing, and anybody
who learns its address can post to it.

Signing closes that. It is off until a destination has a secret, because
turning it on for a receiver that is not checking yet would only break
deliveries.

## What a signed delivery carries

```
X-Charon-Signature: t=1700000000,v1=633c7e4aab70fda7cbb8bec33866b5d0708dc9b7019607c7ab8de4835adf588e
```

- `t` is the moment of **this attempt**, in seconds since the epoch. A retry
  an hour later carries the hour-later moment, not the original.
- `v1` is `HMAC-SHA256(secret, t + "." + body)` in lower-case hex.
- `body` is the exact bytes delivered, before any parsing. Verify before you
  decode json, not after.
- `v1` appears **once per secret** the destination signs with. Accept the
  delivery if any of them matches.

Nothing about this is configurable. A format a receiver has already written
code against cannot be improved without breaking every receiver.

## Setting it up

```sh
charon sign generate
```

That prints a secret and the two lines to run. Nothing is stored by it; you
choose where the secret lives.

```sh
export CHARON_SIGNING_BILLING='chsec_...'
charon sign add -destination billing -secret env:CHARON_SIGNING_BILLING
```

The database holds `env:CHARON_SIGNING_BILLING`, never the secret. A dump of it
forges nothing.

### Where a secret can live

| reference | read from |
|---|---|
| `env:NAME` | an environment variable of the process that delivers |
| `file:/path` | a file, re-read on every attempt |

`file:` is what most deployments want. A Docker secret, a Kubernetes Secret
volume and systemd's `LoadCredential` all project a secret into the filesystem,
and updating a Kubernetes Secret rewrites the mounted file in place — so a
rotated secret takes effect on the next attempt with nothing restarted.

That covers Vault, AWS Secrets Manager and the rest as well: each ships an
agent or CSI driver that writes a file, which is the integration.

**`env:` is read by `charon dispatch`, not by `charon serve`.** They are
separate processes with separate environments. A variable set only on the panel
does not sign anything.

## Seeing whether it works

**settings → routes** has a *signing* column: the references a destination
signs with, when each was added, and whether the process that delivers could
read it when it last looked.

That last part is reported rather than checked on the spot. The panel runs in
`charon serve` and the signing runs in `charon dispatch`, so the panel has no
standing to say whether a secret is readable — it says what the dispatcher
found, or *not read yet* when the dispatcher has not looked. The dispatcher
reports on start, whenever a secret is added or removed, and once a minute
regardless — so a missed announcement cannot leave the panel stale.

A destination that is routed and signs with nothing is listed apart, under
*Delivered without a signature*.

**test** beside a route sends one synthetic event to that destination. It is
recorded and delivered by the dispatcher like any other, so what arrives is
signed exactly as a real delivery would be — the panel could not have signed it
itself. The event page then shows the attempt, the response, and what it went
out signed with.

`charon route list` says the same in a column:

```
stripe    billing    http    enabled   signed(2)   https://billing.internal/hook
github    audit      http    enabled   unsigned    https://audit.internal/github
```

## Rotating

A destination signs with every secret it has, so both are live at once and
neither side has to change at the same instant as the other.

```sh
charon sign add -destination billing -secret env:CHARON_SIGNING_BILLING_NEXT
# deliveries now carry v1= twice; update the receiver to accept either
charon sign remove -destination billing -secret env:CHARON_SIGNING_BILLING
```

`charon sign list` shows when each was added. When it is safe to drop the old
one is something only the receiver knows, so Charon does not guess.

Each attempt records what it went out signed with, shown on the event page, so
after a rotation it is still possible to say which secrets a given delivery
carried rather than which ones are configured now.

## When a secret cannot be read

The attempt fails and **nothing is delivered**. Delivering unsigned to a
receiver that has started checking is the outage signing exists to prevent, and
delivering unsigned to one that has not is a downgrade nobody would notice.

The attempt is retried, because what is missing is put there by a deploy rather
than by anything Charon can do, and the delivery should still be waiting when
it arrives. The recorded reason names the reference that failed.

## Verifying, on the receiving side

Compare in constant time. A byte-by-byte comparison that returns early leaks
the secret to anybody willing to measure.

### Go

```go
func valid(header string, body, secret []byte, tolerance time.Duration) bool {
	var moment string
	var digests []string
	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch key {
		case "t":
			moment = value
		case "v1":
			digests = append(digests, value)
		}
	}

	seconds, err := strconv.ParseInt(moment, 10, 64)
	if err != nil {
		return false
	}
	if drift := time.Since(time.Unix(seconds, 0)); drift > tolerance || drift < -tolerance {
		return false
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(moment + "."))
	mac.Write(body)
	expected := mac.Sum(nil)

	for _, digest := range digests {
		given, err := hex.DecodeString(digest)
		if err == nil && hmac.Equal(given, expected) {
			return true
		}
	}
	return false
}
```

### Node

```js
const crypto = require('node:crypto')

function valid(header, body, secret, toleranceSeconds = 300) {
  const parts = header.split(',').map((p) => p.split('='))
  const moment = parts.find(([k]) => k === 't')?.[1]
  const digests = parts.filter(([k]) => k === 'v1').map(([, v]) => v)
  if (!moment || digests.length === 0) return false

  if (Math.abs(Date.now() / 1000 - Number(moment)) > toleranceSeconds) return false

  const expected = crypto
    .createHmac('sha256', secret)
    .update(moment + '.')
    .update(body)
    .digest()

  return digests.some((digest) => {
    const given = Buffer.from(digest, 'hex')
    return given.length === expected.length && crypto.timingSafeEqual(given, expected)
  })
}
```

`body` must be the raw request body. In Express that means
`express.raw({ type: '*/*' })`, not `express.json()`.

### Python

```python
import hashlib, hmac, time

def valid(header: str, body: bytes, secret: bytes, tolerance: int = 300) -> bool:
    parts = [p.split("=", 1) for p in header.split(",") if "=" in p]
    moment = next((v for k, v in parts if k == "t"), None)
    digests = [v for k, v in parts if k == "v1"]
    if not moment or not digests:
        return False

    if abs(time.time() - int(moment)) > tolerance:
        return False

    expected = hmac.new(secret, f"{moment}.".encode() + body, hashlib.sha256).hexdigest()
    return any(hmac.compare_digest(expected, d) for d in digests)
```

In Flask, `request.get_data()`. In Django, `request.body`.

## Test vectors

If your verifier reproduces these, it agrees with Charon.

| | |
|---|---|
| secret | `chsec_test` |
| `t` | `1700000000` |
| body | `{"id":"evt_1"}` |
| signed | `1700000000.{"id":"evt_1"}` |
| `v1` | `633c7e4aab70fda7cbb8bec33866b5d0708dc9b7019607c7ab8de4835adf588e` |

From a shell, to check your own implementation against something neither side
wrote:

```sh
printf %s '1700000000.{"id":"evt_1"}' | openssl dgst -sha256 -hmac 'chsec_test' -hex
```

## Charon in front of Charon

The `charon` preset checks this format, so one Charon delivering to another
verifies its own signature:

```sh
charon verify set -provider upstream -preset charon -secret-env CHARON_SIGNING_BILLING
```

That is also how the format is tested here — both sides are in the same
repository, so it is proven rather than described.
