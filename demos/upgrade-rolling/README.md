# Rolling Dataplane Upgrade Demo

This demo installs substrate from zero on a throwaway kind cluster at one
build version, runs a stateful counter actor, then rolls the dataplane to a
second build version with `kubectl ate upgrade run` — and proves the actor's
durable state survives the roll: the counter's "preserved file counter"
keeps growing across suspend, roll, and resume. (The "preserved memory
count" resets: the demo template suspends with a Data-scope snapshot,
`snapshotsConfig.onCommit: Data`, which saves durable-dir state and lets the
process exit. Full-scope snapshots, as the template's `onPause: Full` uses,
carry memory too.) The demo also exercises rollback and cleanup.

## The model outside the demo

A substrate version is a git checkout. You install from a release checkout
(`./hack/install-ate.sh --deploy-ate-system`), and an upgrade is checking out
the next release and running one command:

```bash
git checkout <new-release>
./hack/install-ate.sh --upgrade-ate-system
```

The version stamp comes from `git describe`, so the checkout is the release.
`--upgrade-ate-system` stages the new CRDs, controller, and atelet, then
builds the upgrade driver and rolls every node, then finishes ate-api and
atenet (steps 4, 6, and 6b of the runbook below, in that order). Your only
job during the roll is suspending your own actors at your own pace: the
driver waits at each node until no RUNNING or PAUSED actor remains on it,
and never force-suspends anything. Rollback (step 8) and cleanup (step 9)
stay separate commands on purpose: one is an exit you hope not to need, the
other destroys the rollback path once the new version has soaked.

The demo compresses the two checkouts into one source tree: both versions
are built from the **same checkout** and differ only in the `-ldflags` build
version stamp (`internal/version.Version`, set through the Makefile's
`VERSION` variable). The runbook also runs the staged pieces one at a time
instead of through `--upgrade-ate-system`, so each stage is observable. The
stamp, not the checkout, is what keys everything:

- The atelet DaemonSet and each WorkerPool's worker Deployment are rendered
  per version (`ate.dev/substrate-version` label on the objects, their
  selectors, and their pod nodeSelectors), so two versions coexist on
  disjoint node sets.
- atecontroller only creates/mutates worker sets matching its **own** build
  version (hands-off invariant). Old sets are never touched — retiring them
  is the upgrade driver's job.
- `kubectl ate upgrade` moves capacity node by node by flipping the node's
  `ate.dev/substrate-version` label. Actors are never force-suspended: the
  driver drains workers (no new assignments) and then waits until the node
  has no RUNNING or PAUSED actor, i.e. until the customer's actors have left
  at their own pace.

## Scripted run (kind only)

```bash
./run-kind.sh
```

The script performs the whole runbook below against a dedicated kind cluster
named `ate-roll-demo` and deletes that cluster on exit (`KEEP_CLUSTER=true`
keeps it for inspection). It never touches GKE or any cloud resource.

Knobs (environment variables): `VERSION_A` / `VERSION_B` (build version
stamps, default `v0.0.0-roll-a` / `v0.0.0-roll-b`), `ROUTER_PORT` (local
port-forward port, default 18080), `DO_ROLLBACK` (also demo rollback and
re-upgrade, default true), `SHOW_DRAIN_GATE` (demo that a live actor blocks
the roll, default true).

Expect roughly 15–25 minutes end to end; the two `ko` build rounds dominate.

## Manual runbook (kind, from zero)

Prerequisites: docker, go, kubectl, jq, curl. `kind` and `ko` are fetched by
the repo's `hack/run-tool.sh` wrappers automatically.

### 1. Pick two versions and create the cluster

```bash
export VERSION_A=v0.0.0-roll-a VERSION_B=v0.0.0-roll-b
KIND_CLUSTER_NAME=ate-roll-demo ./hack/create-kind-cluster.sh
```

The versions must be valid Kubernetes label values (the installer rejects
anything `internal/versionlabel` would have to rewrite). Real deployments use
`git describe` output, which qualifies; the point of pinning two synthetic
values here is to get two distinct "releases" out of one checkout.

### 2. Install version A

```bash
VERSION=$VERSION_A KIND_CLUSTER_NAME=ate-roll-demo \
  ./hack/install-ate-kind.sh --deploy-ate-system --rollout-timeout=300s
VERSION=$VERSION_A KIND_CLUSTER_NAME=ate-roll-demo \
  ./hack/install-ate-kind.sh --deploy-demo-counter
```

`VERSION` rides the environment into `make ldflags`, so the same value is
stamped into every binary `ko` builds *and* rendered into the versioned
manifests. After this step the cluster has: nodes labeled
`ate.dev/substrate-version=$VERSION_A`, a DaemonSet `atelet-<suffix-A>`
running on them, and a worker Deployment `counter-<suffix-A>` for the demo
WorkerPool. (`<suffix>` is the version lowercased with non-alphanumerics
mapped to `-`.)

`--deploy-demo-counter` waits for the versioned worker set (by label) and for
`actortemplate/counter` to become Ready before returning.

### 3. Create an actor and drive traffic

```bash
go install ./cmd/kubectl-ate
kubectl ate create atespace roll
kubectl ate create actor c1 -a roll --template ate-demo-counter/counter

kubectl port-forward -n ate-system svc/atenet-router 8000:80 &
curl -H "Host: c1.roll.actors.resources.substrate.ate.dev" http://localhost:8000
```

Each request increments both counters; note the "preserved file counter" —
continuity of that number is the proof that the actor's durable state
survives the roll (the memory count resets at each Data-scope suspend, see
the intro). (The driver binary's own version need not match the cluster's;
it only reads and flips labels.)

### 4. Stage the version-B control plane (CRDs, controller, atelet)

The upgrade proposal's order matters here: ate-api and atenet must NOT roll
to B yet, or you open a new-server/old-dataplane window. Stage only the
pieces the dataplane roll needs:

```bash
VERSION=$VERSION_B KIND_CLUSTER_NAME=ate-roll-demo \
  ./hack/install-ate-kind.sh --deploy-ate-controller --rollout-timeout=300s
VERSION=$VERSION_B KIND_CLUSTER_NAME=ate-roll-demo \
  ./hack/install-ate-kind.sh --deploy-atelet --rollout-timeout=300s
```

The first command re-applies the CRDs, publishes the B default worker
images, and rolls ate-controller to B; the B controller renders a
`counter-<suffix-B>` worker set whose pods stay Pending (no node carries the
B label yet). The second creates the `atelet-<suffix-B>` DaemonSet, which
schedules zero pods for the same reason. Everything at version A keeps
running untouched: the installer only labels nodes that are *missing* the
version label, and the B controller's hands-off invariant keeps it away from
the A worker sets.

(Outside a demo, `--upgrade-ate-system` runs steps 4, 6, and 6b as one
command in this exact order.)

### 5. Customer drains at their own pace

The roll in step 6 will not proceed past a node while any RUNNING or PAUSED
actor occupies it. In production you wait for actors to go idle; in the demo
you are the customer:

```bash
kubectl ate suspend actor c1 -a roll
```

(To see the gate in action, run step 6 with `--drain-timeout 15s` *before*
suspending: the passive wait times out on the RUNNING actor and the node is
left untouched.)

### 6. Roll the nodes

```bash
kubectl ate upgrade run --target-version $VERSION_B
kubectl ate upgrade status
```

Per node, one at a time, the driver: drains the node's workers via the
DrainWorker RPC (a drain failure aborts before the cluster is touched);
waits for zero RUNNING/PAUSED actors; flips the node's
`ate.dev/substrate-version` label, which makes the DaemonSet controller swap
the atelet pods automatically; deletes the emptied old-version worker pods —
the flip alone never evicts them (node affinity is scheduling-time only), and
flipping first keeps the old set's replacement pods Pending instead of
letting one reseat on the node; and waits for the new atelet and the B worker
pods to be Ready.

After the pass, the driver re-lists and keeps rolling until nothing is left
to convert, so nodes that joined mid-roll (born at the old pool label) or
reverted (a recreated node comes back at the pool label) are picked up in
the same invocation.

### 6b. Finish the control plane

With every node (and so every worker) at B, rolling the servers can no
longer create the new-server/old-dataplane mix:

```bash
VERSION=$VERSION_B KIND_CLUSTER_NAME=ate-roll-demo \
  ./hack/install-ate-kind.sh --deploy-ate-apiserver --rollout-timeout=300s
VERSION=$VERSION_B KIND_CLUSTER_NAME=ate-roll-demo \
  ./hack/install-ate-kind.sh --deploy-atenet --rollout-timeout=300s
```

### 7. Verify

```bash
curl -H "Host: c1.roll.actors.resources.substrate.ate.dev" http://localhost:8000
```

The request auto-resumes the actor onto a version-B worker and the file
counter continues from where it left off. `kubectl get pods -n ate-demo-counter
-L ate.dev/substrate-version` shows the B pods Running and the A pods Pending
— those Pending pods are deliberate, see the next step.

### 8. Optional rollback

The A worker Deployments were never scaled down or deleted, so their Pending
pods are the rollback spring: relabeling a node back lets the scheduler seat
them again. Rollback is the same loop in reverse:

```bash
kubectl ate suspend actor c1 -a roll
kubectl ate upgrade rollback --target-version $VERSION_A
```

### 9. Cleanup after soak

Once B has soaked and rollback is off the table, retire version A: its
worker sets (the controller never deletes sets — hands-off holds forever)
and its now-idle atelet DaemonSet go together:

```bash
kubectl ate upgrade cleanup --version $VERSION_A
```

`cleanup` refuses to run while any A pod is Running or any node still carries
the A label.

### 10. Teardown

```bash
./hack/kind.sh delete cluster --name ate-roll-demo
```

## GKE runbook (two real checkouts)

On GKE the model from the top of this README runs literally: each version is
a git checkout (normally a release tag), the version stamp comes from
`git describe`, and the whole upgrade is one command. Prerequisites: the GKE
installer environment (`.ate-dev-env.sh` with `PROJECT_ID`, `CLUSTER_NAME`,
`KO_DOCKER_REPO`), and cluster credentials in the current kubectl context.

Install from the old release's checkout:

```bash
git checkout <old-release>
./hack/install-ate.sh --deploy-ate-system --rollout-timeout=300s
./hack/install-ate.sh --deploy-demo-counter
```

Besides labeling the existing nodes, the installer stamps
`ate.dev/substrate-version` on the substrate node pool (through
`tools/setup-gcp pool set-version-label`), so nodes born later (autoscaling,
resize, auto-repair) come up labeled instead of idling unlabeled.

Upgrade:

```bash
git checkout <new-release>
./hack/install-ate.sh --upgrade-ate-system
```

Suspend your actors at your own pace while it runs; the driver waits at each
node. On GKE the command additionally wraps the roll in node pool
operations, in this order and on purpose:

1. **Autoscaler off for the roll window.** While the pool label is still the
   old version, the autoscaler works against the roll: each converted node's
   deleted workers reappear as Pending pods (kept for rollback), they fit
   the pool's node template, and the autoscaler adds old-version nodes to
   seat them, rebuilding the side being retired. The previous min/max are
   restored at the end.
2. **The roll**, node by node, converging over nodes that join or revert
   mid-roll (a node recreated by auto-repair or auto-upgrade comes back at
   the pool's label, empty; the driver's next pass converts it again).
3. **Pool label moves to the new version, only now.** GKE applies pool label
   updates in place to every existing node, so doing this earlier would flip
   the whole fleet at once — the exact thing the per-node roll exists to
   avoid. After the roll it is a no-op on existing nodes and only changes
   what future nodes are born with. The update replaces the full user label
   set, which is why the tooling merges with the pool's existing labels
   (e.g. `ate.dev/sandboxClass`) instead of sending one key.

The staged new-version capacity cannot leak out early through autoscaling:
the cluster autoscaler only simulates against existing pools' templates, and
GKE node auto-provisioning only creates a pool carrying a custom label when
the pod has both a nodeSelector and a toleration for that key — worker pods
carry only the nodeSelector. Do not add a toleration for
`ate.dev/substrate-version` to worker pods; that would open the gate during
staging. Expect noScaleUp events on the staged Pending pods in the meantime;
they are noise.

Other differences from the kind flow:

- **Nodes joining outside a roll** must carry the version label to host the
  dataplane; the pool label guarantees that on GKE. On other platforms, set
  it at the node pool or kubelet level
  (`--node-labels=ate.dev/substrate-version=<version>`). The value picks the
  side the node serves: the fleet's version in steady state, the target
  version to add capacity during a roll. An unlabeled node is safe but idle.
- **Many nodes.** `upgrade run` is one node at a time by default; use
  repeated `--node` flags to control order or to canary a single node
  (explicit `--node` lists skip the converge passes).
- **Worker images.** The in-tree demos leave `spec.workerImage` unset and
  ride the compiled-in versioned default: install-ate.sh stamps the default
  registry (`KO_DOCKER_REPO`) into the controller via -ldflags and publishes
  the ateom images at each version, so an image-less pool resolves to
  `<registry>/ateom-<class>:<version>` per side of the roll. A pool that
  pins `spec.workerImage` explicitly rolls both sides with that exact image
  instead. Plain `go build` binaries carry no default registry, and
  image-less pools then fail reconcile with an error asking for an explicit
  `workerImage`.
- **Driver endpoint.** If the ate-api endpoint is not reachable through the
  default resolution, pass it via
  `ATE_UPGRADE_DRIVER_FLAGS="--endpoint=<host:port>"`.
