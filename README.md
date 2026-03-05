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

1. **proxy-agent** runs in the customer's cluster (deployed via Sveltos pull
   mode, Helm, or any other method).
2. The agent initiates an **outbound WebSocket** connection to **proxy-server**
   in the management cluster. No inbound firewall rules are required on the
   customer side.
3. The agent authenticates with a **pre-shared token** mapped to a cluster ID.
4. When ArgoCD makes a request to
   `http://proxy-server:8080/tunnel/{cluster-id}/api/v1/...`, the server:
   - Validates the proxy token (Authorization header)
   - **Strips the Authorization header** so no management-side credentials
     enter the tunnel
   - Opens a multiplexed connection through the WebSocket tunnel
5. The agent receives the HTTP request through the tunnel:
   - **Reads its Kubernetes ServiceAccount token** from disk
     (`/var/run/secrets/kubernetes.io/serviceaccount/token`)
   - **Injects `Authorization: Bearer <SA-token>`** into the request
   - Forwards the request to the local Kubernetes API (over TLS)
   - Pipes the response back through the tunnel
6. ArgoCD receives the response as if the cluster were directly reachable.

### Zero credentials on the management cluster

The critical design property is that **no customer cluster credentials ever
exist in the management cluster**:

- The proxy server only stores a **proxy token** (for authenticating ArgoCD
  requests) and **agent tokens** (for authenticating agent WebSocket connections).
  Neither of these grants access to the customer's Kubernetes API.
- The agent's **ServiceAccount token** — the actual credential for the
  customer's K8s API — never leaves the customer cluster. It is read from disk
  inside the agent pod and injected into requests locally.
- The agent's RBAC (ClusterRole/ClusterRoleBinding) on the customer cluster
  defines what ArgoCD can do. This is fully controlled by the customer.

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

Multiple TCP connections are multiplexed over a single WebSocket using
connection IDs.

### Security model

- **No credentials on the management cluster**: The agent injects its own
  Kubernetes ServiceAccount token into every proxied request. The management
  cluster never stores, sees, or forwards customer cluster credentials.
- **Outbound-only**: The agent initiates all connections. The customer cluster
  needs no inbound firewall rules.
- **Token authentication**: Agents authenticate with pre-shared tokens during
  the WebSocket handshake. A separate proxy token authenticates ArgoCD requests.
- **Authorization header stripping**: The server strips all Authorization
  headers before forwarding requests through the tunnel, preventing credential
  leakage from the management side.
- **Agent-side TLS**: The agent terminates TLS to the local Kubernetes API
  server using the auto-mounted CA certificate.
- **SSRF protection**: The agent only dials its configured target address,
  preventing the server from directing it to arbitrary endpoints.
- **Constant-time token comparison**: All token validation uses
  `crypto/subtle.ConstantTimeCompare` to prevent timing attacks.

## Quick start

### Prerequisites

- Go 1.22+
- Docker (for building images)
- Helm 3 (for chart deployment)
- kind (for e2e tests)

### Build

```bash
make build          # build binaries to bin/
make docker         # build Docker images
make test           # run unit tests
make test-e2e       # run e2e tests (requires Docker + kind)
```

### Deploy with Helm

There are **two separate Helm charts**, one for each cluster:

**Management cluster** (server + ArgoCD cluster secrets):

```bash
helm install proxy-server deploy/helm/argocd-cluster-proxy-server/ \
  --namespace proxy-system --create-namespace \
  --set proxyToken=<generate-a-random-token> \
  --set argocd.enabled=true \
  --set argocd.namespace=argocd \
  --set 'clusters[0].id=customer-a' \
  --set 'clusters[0].token=<generate-agent-token>'
```

**Customer cluster** (agent — fully automated, only needs the agent token):

```bash
helm install proxy-agent deploy/helm/argocd-cluster-proxy-agent/ \
  --namespace proxy-system --create-namespace \
  --set serverURL=wss://proxy.management.example.com/connect \
  --set token=<same-agent-token-as-above>
```

That's it. The agent chart automatically creates:
- A **ServiceAccount** (whose token authenticates to the local K8s API)
- A **ClusterRole + ClusterRoleBinding** (default: full access for ArgoCD)
- A **Secret** for the tunnel connection token
- The agent **Deployment** with all volume mounts configured

No manual certificate generation, RBAC setup, or credential management needed.

### Run locally (development)

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

## Configuration

### Server flags

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `-addr` | | `:8080` | Listen address |
| `-clusters` | `CLUSTERS` | | Path to clusters JSON file (or JSON string via env) |
| `-proxy-token` | `PROXY_TOKEN` | | Bearer token required on /tunnel/ requests |
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

### Agent flags

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
| `-log-level` | | `info` | Log level |

## Project structure

```
├── cmd/
│   ├── server/          # proxy-server binary
│   └── agent/           # proxy-agent binary
├── pkg/
│   ├── tunnel/          # multiplexed tunnel protocol over WebSocket
│   ├── server/          # HTTP reverse proxy + WebSocket acceptor
│   └── agent/           # WebSocket dialer + credential-injecting proxy
├── test/e2e/            # end-to-end tests using kind
├── deploy/helm/
│   ├── argocd-cluster-proxy-server/   # Helm chart for management cluster
│   └── argocd-cluster-proxy-agent/    # Helm chart for customer cluster
├── .github/workflows/   # CI/CD pipelines
├── Dockerfile.server
├── Dockerfile.agent
└── Makefile
```

## License

AGPL-3.0 — Cloud Exit B.V.
