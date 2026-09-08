# Install

Charon is one binary and one image. It needs PostgreSQL 18 and nothing else:
no broker, no sidecar, no agent, and no call to any service this project
controls.

```sh
docker pull edersomangelo/charon:alpha
```

`linux/amd64` and `linux/arm64`, built from `scratch`: the binary, the CA
certificates and nothing else. It runs as uid `65534` and needs no root.

## Tags

| tag | |
|---|---|
| `alpha` | the newest pre-release, and it moves |
| `0.1.0-alpha.3` | that one, and it does not |

Pin the version anywhere you would be upset to be upgraded without asking.
There is deliberately no `latest`: nothing is released yet that somebody should
get by not choosing.

## Running it

One image, two commands. `serve` receives webhooks and serves the panel;
`dispatch` delivers them. They share nothing but the database.

```sh
docker run --rm \
  -e CHARON_DATABASE_URL='postgres://charon:charon@db:5432/charon?sslmode=disable' \
  -p 8080:8080 edersomangelo/charon:alpha

docker run --rm \
  -e CHARON_DATABASE_URL='postgres://charon:charon@db:5432/charon?sslmode=disable' \
  edersomangelo/charon:alpha dispatch
```

PostgreSQL 18 is yours to provide. Charon does not ship one and does not care
where it comes from.

## Kubernetes

The connection string and every provider secret belong in a Secret. The
database holds the *name* of the variable a secret is read from, never the
secret, so both processes need the variable itself.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: charon
type: Opaque
stringData:
  CHARON_DATABASE_URL: postgres://charon:...@postgres:5432/charon?sslmode=disable
  CHARON_SECRET_STRIPE: whsec_...
```

`serve` applies pending migrations as it starts, so bring it up before
`dispatch`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: charon-server
spec:
  replicas: 2
  selector:
    matchLabels: { app: charon-server }
  template:
    metadata:
      labels: { app: charon-server }
    spec:
      containers:
        - name: charon
          image: edersomangelo/charon:0.1.0-alpha.3
          args: ["serve"]
          envFrom:
            - secretRef: { name: charon }
          env:
            - name: CHARON_METRICS_ADDR
              value: ":9090"
          ports:
            - { name: http, containerPort: 8080 }
            - { name: metrics, containerPort: 9090 }
          livenessProbe:
            httpGet: { path: /healthz, port: http }
          readinessProbe:
            httpGet: { path: /readyz, port: http }
          securityContext:
            runAsNonRoot: true
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: charon-dispatcher
spec:
  replicas: 1
  selector:
    matchLabels: { app: charon-dispatcher }
  template:
    metadata:
      labels: { app: charon-dispatcher }
    spec:
      containers:
        - name: charon
          image: edersomangelo/charon:0.1.0-alpha.3
          args: ["dispatch"]
          envFrom:
            - secretRef: { name: charon }
          securityContext:
            runAsNonRoot: true
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
---
apiVersion: v1
kind: Service
metadata:
  name: charon
spec:
  selector: { app: charon-server }
  ports:
    - { name: http, port: 80, targetPort: http }
```

Point an Ingress at that Service. `POST /webhooks/{tenant}/{provider}` is the
only path that has to be reachable from outside; the panel can be kept
internal.

**Replicas.** `serve` is stateless and scales freely — migrations take an
advisory lock, so several starting at once is not a problem. `dispatch` can be
scaled too: deliveries are claimed under a lease with `for update skip locked`,
so two dispatchers never take the same one.

**Metrics** are served on a listener of their own, off unless
`CHARON_METRICS_ADDR` names one, because a scrape reports every tenant's counts
and does not belong on the port the internet posts to.

The first operator is made with a Job, or by `kubectl exec` into a running pod:

```sh
kubectl exec deploy/charon-server -- \
  /charon user add -email you@example.com -password '...'
```
