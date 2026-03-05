# argocd-cluster-proxy

Reverse tunnel proxy that allows a managed ArgoCD instance to deploy to
air-gapped customer clusters **without storing any customer cluster credentials
in the management cluster**.

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                      Management Cluster                          │
│                                                                  │
│  ┌──────────┐   HTTP    ┌──────────────┐   WebSocket tunnel      │
│  │  ArgoCD  │──────────▶│ proxy-server │◀───────────────────┐    │
│  └──────────┘           └──────────────┘                    │    │
│                          /tunnel/{id}/*                     │    │
│                     (strips Authorization)                  │    │
│                                                             │    │
└─────────────────────────────────────────────────────────────┼────┘
                                                              │
                              Firewall (outbound only) ───────┼────
                                                              │
┌─────────────────────────────────────────────────────────────┼────┐
│                      Customer Cluster (air-gapped)          │    │
│                                                             │    │
│  ┌─────────────┐   TLS    ┌──────────────────┐              │    │
│  │  K8s API    │◀─────────│   proxy-agent    │──────────────┘    │
│  │  Server     │  + SA    │  (connects out)  │                   │
│  └─────────────┘  token   └──────────────────┘                   │
│                             Injects its own                      │
│                             ServiceAccount token                 │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

### How it works

1. **proxy-agent** runs in the customer's cluster and initiates an **outbound
   WebSocket** connection to **proxy-server** in the management cluster. No
   inbound firewall rules are required on the customer side.
2. The agent authenticates with a **pre-shared token** mapped to a cluster ID.
3. When ArgoCD makes a request to
   `http://proxy-server:8080/tunnel/{cluster-id}/api/v1/...`, the server:
   - Validates the proxy token (Authorization header)
   - **Strips the Authorization header** so no management-side credentials
     enter the tunnel
   - Opens a multiplexed connection through the WebSocket tunnel
4. The agent receives the HTTP request through the tunnel:
   - **Reads its Kubernetes ServiceAccount token** from disk
   - **Injects `Authorization: Bearer <SA-token>`** into the request
   - Forwards the request to the local Kubernetes API (over TLS)
   - Pipes the response back through the tunnel
5. ArgoCD receives the response as if the cluster were directly reachable.

### Zero credentials on the management cluster

| Component | Has customer K8s credentials? |
|-----------|-------------------------------|
| ArgoCD | No — only has a proxy token |
| proxy-server | No — passes requests through the tunnel |
| proxy-agent | Yes — reads its local SA token on each request |

The agent re-reads the token from disk on every request, supporting automatic
Kubernetes token rotation via projected volumes.

### Security

- **Constant-time auth** — SHA-256 + `subtle.ConstantTimeCompare`, all clusters
  checked (no early return) to prevent timing attacks
- **Header stripping** — Authorization headers removed before tunneling,
  including RFC 7230 hop-by-hop headers
- **SSRF protection** — agent only dials its configured target address
- **TLS enforcement** — agent rejects plaintext `ws://` unless explicitly allowed;
  minimum TLS 1.2 on all connections
- **Distroless images** — non-root, read-only rootfs, all capabilities dropped
- **Connection limits** — max 1024 concurrent tunnel connections per session
- **Rate limiting** — optional per-server request throttling
- **Pong timeout** — dead WebSocket peers detected and cleaned up within 45s

### Tunnel protocol

The tunnel uses a custom binary protocol over a single WebSocket connection:

| Type | Name | Direction | Description |
|------|------|-----------|-------------|
| 0x01 | Connect | Server → Agent | Open TCP connection to target address |
| 0x02 | Connected | Agent → Server | Connection established |
| 0x03 | Data | Bidirectional | Raw payload bytes |
| 0x04 | Error | Bidirectional | Error message |
| 0x05 | Close | Bidirectional | Close connection |
| 0x06 | Ping | Bidirectional | Keepalive |
| 0x07 | Pong | Bidirectional | Keepalive ack |

Frame format: `[type:1 byte][connID:4 bytes big-endian][payload:N bytes]`

HTTP upgrade requests (SPDY, WebSocket) are fully supported for `kubectl exec`,
`kubectl logs --follow`, and `kubectl port-forward`.

## Quick start

### Prerequisites

- Go 1.23+
- Docker (for building images)
- Helm 3 (for chart deployment)
- kind (for e2e tests)

### Deploy with Helm

There are **two separate Helm charts**, one for each cluster:

**Management cluster** (server + ArgoCD cluster secrets):

```bash
helm install proxy-server \
  oci://ghcr.io/cloud-exit/charts/argocd-cluster-proxy-server \
  --namespace proxy-system --create-namespace \
  --set proxyToken=<generate-a-random-token> \
  --set argocd.enabled=true \
  --set argocd.namespace=argocd \
  --set 'clusters[0].id=customer-a' \
  --set 'clusters[0].token=<generate-agent-token>'
```

**Customer cluster** (agent — fully automated, only needs the agent token):

```bash
helm install proxy-agent \
  oci://ghcr.io/cloud-exit/charts/argocd-cluster-proxy-agent \
  --namespace proxy-system --create-namespace \
  --set serverURL=wss://proxy.management.example.com/connect \
  --set token=<same-agent-token-as-above>
```

The agent chart automatically creates:
- A **ServiceAccount** (whose token authenticates to the local K8s API)
- A **ClusterRole + ClusterRoleBinding** (default: full access for ArgoCD)
- A **Secret** for the tunnel connection token
- The agent **Deployment** with all volume mounts configured

Setting `argocd.enabled=true` on the server chart creates ArgoCD cluster secrets
so ArgoCD discovers the proxied clusters automatically.

### Verify the connection

```bash
# On the management cluster — readyz returns "ok" when at least one agent is connected
kubectl -n proxy-system port-forward svc/proxy-server 8080
curl http://localhost:8080/readyz
# ok

# List namespaces on the customer cluster through the proxy
curl -H "Authorization: Bearer <proxy-token>" \
  http://localhost:8080/tunnel/customer-a/api/v1/namespaces
```

## ArgoCD integration

When the server Helm chart is installed with `argocd.enabled=true`, it creates
an ArgoCD cluster secret per entry in the `clusters` list:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: proxy-server-cluster-customer-a
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: cluster
type: Opaque
stringData:
  name: customer-a
  server: "http://proxy-server.proxy-system.svc:8080/tunnel/customer-a"
  config: |
    {
      "bearerToken": "<proxy-token>",
      "tlsClientConfig": { "insecure": true }
    }
```

ArgoCD picks this up automatically and treats each proxied cluster as a regular
remote cluster. The `bearerToken` authenticates to the proxy server — it is
stripped before tunneling and the agent injects the real K8s SA token on the
customer side. `insecure: true` is safe here because traffic stays on the
cluster-internal network.

### Example: deploying an application through the proxy

Once the server and agent are running, create an ArgoCD Application that targets
the proxied cluster. No special configuration is needed — ArgoCD sees it as any
other cluster:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: guestbook
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/argoproj/argocd-example-apps.git
    targetRevision: HEAD
    path: guestbook
  destination:
    # Use the cluster name registered by the server chart.
    name: customer-a
    namespace: default
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

Alternatively, reference the cluster by its proxy URL directly:

```yaml
  destination:
    server: http://proxy-server.proxy-system.svc:8080/tunnel/customer-a
    namespace: default
```

Both forms work identically. Using `name:` is preferred because it decouples
the Application manifest from the internal proxy URL.

### Multi-cluster example

Register multiple customer clusters in a single server deployment:

```bash
helm install proxy-server \
  oci://ghcr.io/cloud-exit/charts/argocd-cluster-proxy-server \
  --namespace proxy-system --create-namespace \
  --set proxyToken=<proxy-token> \
  --set argocd.enabled=true \
  --set 'clusters[0].id=staging' \
  --set 'clusters[0].token=<staging-token>' \
  --set 'clusters[1].id=production' \
  --set 'clusters[1].token=<production-token>'
```

Then create an ApplicationSet to deploy across all proxied clusters:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: platform-apps
  namespace: argocd
spec:
  generators:
    - clusters:
        selector:
          matchLabels:
            argocd.argoproj.io/secret-type: cluster
  template:
    metadata:
      name: "platform-{{name}}"
    spec:
      project: default
      source:
        repoURL: https://github.com/your-org/platform-apps.git
        targetRevision: HEAD
        path: "envs/{{name}}"
      destination:
        name: "{{name}}"
        namespace: platform
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
```

## High availability

### How sessions work

Each agent opens a single WebSocket tunnel to the server. The server keeps an
in-memory registry mapping cluster IDs to active tunnel sessions. Proxy requests
for a cluster are routed through that cluster's session. This means:

- A cluster's tunnel exists on exactly **one** server pod.
- If a proxy request hits a server pod that doesn't hold the tunnel, it returns 502.
- If two agents connect with the same token, the second replaces the first
  (last-writer-wins).

### Server HA

The server does not yet support shared session state across replicas. Run
**one replica** and rely on Kubernetes for fast restarts:

```bash
helm install proxy-server \
  oci://ghcr.io/cloud-exit/charts/argocd-cluster-proxy-server \
  --namespace proxy-system --create-namespace \
  --set replicas=1 \
  --set pdb.enabled=true \
  --set pdb.minAvailable=0 \
  --set proxyToken=<token> \
  --set 'clusters[0].id=customer-a' \
  --set 'clusters[0].token=<agent-token>' \
  --set argocd.enabled=true
```

Why this works:

- The server is **stateless on disk** — all session state rebuilds automatically
  when agents reconnect after a restart.
- The agent's reconnect loop (exponential backoff, max 60s) re-establishes the
  tunnel within seconds of a server restart.
- `pdb.minAvailable=0` allows rolling updates to proceed (a single-replica PDB
  with `minAvailable=1` would block all evictions).
- The liveness probe (`/healthz`) ensures the pod is restarted if the process
  hangs.

For zero-downtime rolling updates, the server drains in-flight tunnel
connections on shutdown (5s grace period) before the pod terminates.

**Production hardening for the single replica:**

```yaml
# values.yaml
replicas: 1

pdb:
  enabled: true
  minAvailable: 0

resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    memory: 512Mi

topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: topology.kubernetes.io/zone
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels: {}  # auto-filled from chart
```

> **Future work**: Shared session state (e.g. via Redis or gossip protocol)
> would allow running multiple server replicas. Agent connections would be
> balanced across server pods, and any pod could serve proxy requests for any
> cluster.

### Agent HA

Run **one agent replica per customer cluster**. Multiple replicas with the same
token would repeatedly evict each other's sessions:

```bash
helm install proxy-agent \
  oci://ghcr.io/cloud-exit/charts/argocd-cluster-proxy-agent \
  --namespace proxy-system --create-namespace \
  --set replicas=1 \
  --set pdb.enabled=true \
  --set pdb.minAvailable=0 \
  --set serverURL=wss://proxy.management.example.com/connect \
  --set token=<agent-token>
```

The agent handles failures automatically:

| Failure | Recovery |
|---------|----------|
| Agent pod crash | Kubernetes restarts the pod; agent reconnects |
| Server restart | Agent detects the broken WebSocket and reconnects with backoff |
| Network blip | WebSocket ping/pong detects the failure within 45s; agent reconnects |
| Node drain | PDB controls eviction; new pod starts and reconnects |

**Production hardening for the agent:**

```yaml
# values.yaml
replicas: 1

pdb:
  enabled: true
  minAvailable: 0

resources:
  requests:
    cpu: 50m
    memory: 64Mi
  limits:
    memory: 256Mi

maxRetryInterval: "60s"

# Restrict what ArgoCD can do on this cluster.
rbac:
  create: true
  clusterWide: true
  rules:
    - apiGroups: ["*"]
      resources: ["*"]
      verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

### Failure scenarios

| Scenario | Impact | Recovery time |
|----------|--------|---------------|
| Server pod restart | All tunnels drop; ArgoCD gets 502 | ~5-15s (agent reconnect + readiness probe) |
| Agent pod restart | One cluster unreachable; ArgoCD gets 502 | ~5-10s (pod restart + reconnect) |
| Server node failure | All tunnels drop | ~30-60s (pod reschedule + agent reconnect) |
| Agent node failure | One cluster unreachable | ~30-60s (pod reschedule + reconnect) |
| Network partition | Affected tunnels drop | ~45s (pong timeout) + reconnect backoff |

In all cases, ArgoCD retries its sync operations automatically. Brief proxy
outages result in transient sync failures, not data loss.

## Configuration

### Server

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `-addr` | | `:8080` | Listen address |
| `-clusters` | `CLUSTERS` | | Path to clusters JSON file (or JSON string via env) |
| `-proxy-token` | `PROXY_TOKEN` | | Bearer token required on /tunnel/ requests |
| `-rate-limit` | | `0` | Max proxy requests per second (0 = unlimited) |
| `-log-level` | | `info` | Log level (debug, info, warn, error) |

Clusters JSON format:
```json
[
  {
    "id": "customer-a",
    "token": "random-secret-token",
    "targetAddr": "kubernetes.default.svc:443"
  }
]
```

### Agent

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `-server` | `SERVER_URL` | | WebSocket URL of the proxy server |
| `-token` | `TOKEN` | | Pre-shared authentication token |
| `-target` | | `kubernetes.default.svc:443` | Local K8s API address |
| `-sa-token-path` | | `/var/run/secrets/.../token` | Path to SA token for credential injection |
| `-ca-cert` | | `/var/run/secrets/.../ca.crt` | CA cert for local API |
| `-insecure` | | `false` | Skip TLS verification to local API |
| `-plain-target` | | `false` | Connect without TLS (testing only) |
| `-allow-insecure-server` | | `false` | Allow plaintext ws:// (testing only) |
| `-max-retry` | | `60s` | Max reconnect backoff |
| `-health-addr` | | `:8081` | Health/metrics server address |
| `-log-level` | | `info` | Log level |

## Observability

Both components expose Prometheus metrics on `/metrics` and health probes.

### Metrics

| Metric | Component | Type | Description |
|--------|-----------|------|-------------|
| `proxy_requests_total` | server | counter | Proxied requests (by cluster, method, status) |
| `proxy_request_duration_seconds` | server | histogram | Request latency by cluster |
| `tunnel_active_connections` | server | gauge | Active tunnel connections per cluster |
| `agents_connected` | server | gauge | Connected agent count |
| `proxy_rate_limited_total` | server | counter | Rate-limited requests |
| `agent_connected` | agent | gauge | 1 if connected, 0 if not |
| `agent_reconnects_total` | agent | counter | Reconnection attempts |
| `agent_upstream_requests_total` | agent | counter | Requests forwarded to K8s API |
| `agent_upstream_errors_total` | agent | counter | Upstream errors |

### Health probes

| Endpoint | Description |
|----------|-------------|
| `/healthz` | Liveness — always 200 if the process is running |
| `/readyz` | Readiness — 200 when connected (server: >=1 agent, agent: tunnel up), 503 otherwise |

The Helm charts configure Kubernetes liveness and readiness probes against
these endpoints. The agent health server runs on port 8081 by default.

Prometheus scraping is enabled via pod annotations:

```yaml
prometheus.io/scrape: "true"
prometheus.io/port: "8080"   # server
prometheus.io/port: "8081"   # agent
prometheus.io/path: "/metrics"
```

## Development

### Build

```bash
make build          # Build both binaries to bin/
make test           # Unit tests with race detector
make test-e2e       # E2E tests using kind clusters
make lint           # golangci-lint
make docker         # Build container images
```

### Run locally

Terminal 1 — Server:
```bash
cat > /tmp/clusters.json << 'EOF'
[{"id": "dev", "token": "dev-token", "targetAddr": "127.0.0.1:6443"}]
EOF
go run ./cmd/server -addr :8080 -clusters /tmp/clusters.json -log-level debug
```

Terminal 2 — Agent:
```bash
go run ./cmd/agent \
  -server ws://localhost:8080/connect \
  -token dev-token \
  -target kubernetes.default.svc:443 \
  -allow-insecure-server \
  -log-level debug
```

Terminal 3 — Test:
```bash
curl http://localhost:8080/tunnel/dev/api/v1/namespaces
```

### Project structure

```
├── cmd/
│   ├── server/          # proxy-server binary
│   └── agent/           # proxy-agent binary
├── pkg/
│   ├── tunnel/          # multiplexed tunnel protocol over WebSocket
│   ├── server/          # HTTP reverse proxy + WebSocket acceptor
│   ├── agent/           # WebSocket dialer + credential-injecting proxy
│   └── metrics/         # Prometheus metric definitions
├── test/e2e/            # end-to-end tests using kind
├── deploy/helm/
│   ├── argocd-cluster-proxy-server/   # Helm chart for management cluster
│   └── argocd-cluster-proxy-agent/    # Helm chart for customer cluster
├── .github/workflows/   # CI (lint, test, e2e) + Release (Docker, Helm, GHCR)
├── Dockerfile.server
├── Dockerfile.agent
└── Makefile
```

## License

AGPL-3.0 — Cloud Exit B.V.
