# `kubectl-ate`

A Kubernetes-native CLI plugin for managing Substrate Actor and Worker lifecycles.

## Running the CLI

There are two ways to run the tool, depending on whether you are developing locally or installing it permanently.

### 1. Install as a native `kubectl` Plugin
You can use `go install` to compile the tool and place the binary directly into your Go bin directory (which should be in your `$PATH`). Because the source folder is named `kubectl-ate`, Kubernetes will automatically recognize the resulting binary!

```bash
go install ./cmd/kubectl-ate
```
You can now run it seamlessly anywhere as a native Kubernetes command: `kubectl ate <command>`.

### 2. Run directly from source (Development)
If you are testing changes to the codebase, you can bypass compilation and run the CLI directly from the source tree:

```bash
go run ./cmd/kubectl-ate <command>
```

## Connection & Auto Port-Forwarding
By default, `kubectl-ate` will automatically read your `~/.kube/config`, discover the `ate-api-server` pods in your cluster, and establish a temporary background port-forward tunnel to execute gRPC calls securely.

If you prefer to route traffic directly (e.g., through a LoadBalancer or when running natively inside a cluster pod), simply provide the `--endpoint` flag to bypass the tunnel.

## Tracing
The CLI supports on-demand tracing using the `--trace` flag. When enabled, the CLI will generate a trace ID and signal to the server that it wants the request to be traced.

**Prerequisites:**

1. The Google Cloud project must have the **Cloud Trace API** enabled. You can enable it using:
```bash
gcloud services enable cloudtrace.googleapis.com --project=PROJECT_ID
```

2. The GKE cluster must have **Managed OpenTelemetry** enabled. If it is not enabled, you can enable it using the following `gcloud` command:

```bash
gcloud beta container clusters update CLUSTER_NAME \
    --project=PROJECT_ID \
    --managed-otel-scope=COLLECTION_AND_INSTRUMENTATION_COMPONENTS \
    --location=LOCATION
```

## Global Flags
These flags can be appended to any command:

| Flag | Short | Description | Default |
|---|---|---|---|
| `--kubeconfig` | | Path to your kubeconfig file | `~/.kube/config` |
| `--endpoint` | | Manual gRPC endpoint override (e.g., `localhost:8080`) | |
| `--output` | `-o` | Output format (`table`, `json`, `yaml`) | `table` |
| `--trace` | | Enable on-demand tracing for the request | `false` |

---

## Command Reference & Examples

### Getting Resources
List and inspect the state of actors and workers across the cluster.

```bash
# List all actors in a clean table format
kubectl ate get actors

# Get a specific actor by ID and output as raw YAML
kubectl ate get actor <actor-id> -o yaml

# List all physical workers and see which actors are assigned to them
kubectl ate get workers
```

### Actor Lifecycle
Manage the execution state of your workloads. 
*(Note: Actors are identified by a user-provided ID, which must be a valid DNS-1123 label)*

```bash
# Create a new actor deriving from a specific ActorTemplate
kubectl ate create actor my-actor --template=ate-demo-counter/counter-template

# Resume an actor (assigns it to a free worker and restores its state)
kubectl ate resume actor my-actor

# Suspend an actor (snapshots its state to storage and frees the worker)
kubectl ate suspend actor my-actor

# Delete an actor.
kubectl ate delete actor my-actor
```

### Administration & Setup
Commands for bootstrapping the Substrate control plane and debugging local environments.

```bash
# Generate a new CA pool and push it directly to a Kubernetes Secret
kubectl ate admin make-ca-pool \
  --name workerpool-ca-certs \
  --secret-namespace ate-system \
  --ca-id "1"

# Generate a new JWT authority pool and push it to a Kubernetes Secret
kubectl ate admin make-jwt-pool \
  --name session-id-jwt-pool \
  --secret-namespace ate-system \
  --key-id "1"

# DANGEROUS: Completely flush all Actor and Worker tracking state from Redis
kubectl ate admin debug-flush-redis
```
