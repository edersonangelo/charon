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

## What it is made of

One image, two commands. `serve` receives webhooks and serves the panel;
`dispatch` delivers them. They share nothing but the database, and neither
needs to know the other exists.

PostgreSQL 18 is yours to provide. Charon does not ship one and does not care
where it comes from.

`serve` applies pending migrations as it starts and `dispatch` does not, so
start the server first wherever both are run.

## Standalone

One host with Docker and a database. Enough to receive real traffic, and what
most deployments will ever need.

```sh
export CHARON_DATABASE_URL='postgres://charon:charon@db:5432/charon?sslmode=disable'

docker run -d --name charon-server \
  -e CHARON_DATABASE_URL -p 8080:8080 \
  edersomangelo/charon:alpha

docker run -d --name charon-dispatcher \
  -e CHARON_DATABASE_URL \
  edersomangelo/charon:alpha dispatch
```

To keep them up across reboots, a compose file of your own, using the published
image rather than building anything:

```yaml
services:
  charon:
    image: edersomangelo/charon:0.1.0-alpha.3
    environment:
      CHARON_DATABASE_URL: postgres://charon:charon@postgres:5432/charon?sslmode=disable
    env_file:
      - path: .env
        required: false
    ports:
      - "8080:8080"
    depends_on:
      postgres:
        condition: service_healthy
    restart: unless-stopped

  dispatcher:
    image: edersomangelo/charon:0.1.0-alpha.3
    command: ["dispatch"]
    environment:
      CHARON_DATABASE_URL: postgres://charon:charon@postgres:5432/charon?sslmode=disable
    env_file:
      - path: .env
        required: false
    depends_on:
      postgres:
        condition: service_healthy
    restart: unless-stopped

  postgres:
    image: postgres:18-alpine
    environment:
      POSTGRES_USER: charon
      POSTGRES_PASSWORD: charon
      POSTGRES_DB: charon
    volumes:
      - charon-data:/var/lib/postgresql
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U charon"]
      interval: 5s
      retries: 20
    restart: unless-stopped

volumes:
  charon-data:
```

A provider's secret goes in `.env` beside that file, never in the compose file
itself: the database holds the *name* of the variable it is read from, so the
process that reads it needs the variable.

The first operator, once:

```sh
docker exec charon-server /charon user add -email you@example.com -password '...'
```

Then the panel is on `:8080`, and so is
`POST /webhooks/{tenant}/{provider}`.

## Kubernetes

A cluster you already run. Worth the extra pieces when the two processes should
scale apart, or when the secrets and the rollout already live there.

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
