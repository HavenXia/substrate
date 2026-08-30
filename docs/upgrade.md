# Rolling upgrade runbook

This runbook upgrades a running Agent Substrate install to a new
build version without losing actor state, one node at a time, using
only `kubectl`, `kubectl ate`, `go run ./cmd/ate-setup`, `jq`,
`grpcurl`, and (on GKE) `gcloud`. There is no upgrade driver in this
release; you execute the roll yourself, and all upgrade state lives
in cluster objects, so you can stop, inspect, and rerun at any point.

The order is ate-controller first, then the dataplane, then the rest
of the control plane. The controller is the dataplane's controller:
it crosses to the new build before anything it manages, so a render
change lands as one verified event before any node moves, and the
cloned pools are rendered by the controller that will own them. The
dataplane then rolls node by node behind the version label, and
ate-api-server and atenet follow only once every actor is on the new
side.

## What this runbook assumes

The cluster was installed from a versioned build: nodes carry the
`ate.dev/substrate-version` label, the atelet DaemonSet name carries
a version suffix, and the installed ate-api-server serves
`DrainWorker`. Clusters installed from a pre-versioning build are out
of scope (the DaemonSet selector is immutable); they need a fresh
install.

## Ground rules

Six mistakes break a roll.

1. **Flip the node's version label before deleting its old worker
   pods**, or the old pool reschedules replacements onto the node.
2. **Do not touch the GKE node pool's label until every node is
   rolled**: a pool label update applies in place to every node, so
   the whole fleet flips at once, with no drain and no pacing.
3. **Do not edit a serving pool's pod-shape fields** (`workerImage`,
   `sandboxClass`, `template`): the controller rolls its Deployment
   through live actors. `sandboxConfigName` is a quieter hazard (the
   edit restarts nothing but changes what future cold boots fetch).
   Version moves go through a second pool; `spec.replicas` is exempt.
4. **Keep the fleet quiet until step 6 is done: no creating, resuming,
   or pausing actors.** New work extends the roll, a pause pins an
   actor to its node invisibly, and a cold boot mid-roll can pair
   sandbox binaries across versions. Suspending is always safe; it is
   the mechanism the roll is built on.
5. **Draining does not evict; suspend is the only safe way off a
   worker.** A drained worker hosts its actor until the actor
   suspends; deleting a pod that still hosts an actor crashes it
   terminally. A paused actor is worse: its snapshot is local to its
   node, so the preflight suspends every paused actor first.
6. **Never restage micro-VM sandbox assets over the same bucket
   paths** (for example by rerunning `hack/install-microvm-deps.sh
   --install`): every pre-upgrade restore would fail its checksum.
   Stage new assets under a new bucket prefix, with a new
   SandboxConfig for the new pool. The default gVisor config is safe;
   it points at immutable upstream URLs.

## Preflight

The roll uses these names throughout; grab them up front:

| name | what it is | how to get it |
|---|---|---|
| `$CLUSTER`, `$ZONE` | the GKE cluster and its location | `gcloud container clusters list` |
| `$NODEPOOL` | the GKE node pool | `gcloud container node-pools list --cluster $CLUSTER --zone $ZONE` |
| `$OLD` | the installed version label value | `kubectl get ds -n ate-system -l app=atelet -L ate.dev/substrate-version` prints one DaemonSet before the upgrade; its label value is `$OLD` |
| `$NEW` | the new version label value | read off the cluster in step 3, once the new atelet DaemonSet exists; never off `git describe` |
| `$NS`, `$OLD_POOL` | namespace and name of a serving WorkerPool | `kubectl get workerpools -A` |
| `$NEW_POOL` | the clone's name | you pick it in step 4, for example `counter-v2` |
| `$NODE` | the node being rolled | picked per iteration in step 5 from `kubectl get nodes` |

A cluster usually serves more than one WorkerPool, and every serving
pool moves in the same upgrade: step 4 clones each of them, the
per-node roll in step 5 covers all pools on a node together, and the
retire at the end deletes each old pool. Where the runbook says
`$OLD_POOL`, read "each serving pool".

```bash
# Every node carries the same version label; that value is $OLD.
kubectl get nodes -L ate.dev/substrate-version

# EVERY serving pool carries the version pin at $OLD. An unpinned
# pool cannot participate: after a node flips, its deleted pods would
# reschedule right back onto the flipped node.
kubectl get workerpools -A \
  -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,PIN:.spec.template.nodeSelector.ate\.dev/substrate-version'

# The old pool is healthy (READY == DESIRED).
kubectl -n $NS get workerpool $OLD_POOL

# The control plane answers.
kubectl ate get workers
```

If a pool shows `<none>` in PIN, fix it now: suspend every actor it
serves, then add
`spec.template.nodeSelector: {ate.dev/substrate-version: "$OLD"}`
(the edit rolls its Deployment, harmless once nothing is assigned).

Then suspend every paused actor in the fleet (`kubectl ate get actors
-A` may show no ACTOR_STATE_PAUSED row). A pause clears the worker
assignment, so no listing names the node holding the local snapshot;
this must happen fleet-wide before anything moves.

## Upgrade

### 1. Park the autoscaler (GKE)

During the roll, the old pool's displaced pods sit Pending on
purpose: they are the rollback reserve. The autoscaler reads Pending
pods as demand and would add nodes chasing pods that must never
schedule, so park it. Save its config first; step 7 restores it.

```bash
gcloud container node-pools describe $NODEPOOL --cluster $CLUSTER --zone $ZONE \
  --format='json(autoscaling)'   # save for step 7
gcloud container clusters update $CLUSTER --zone $ZONE \
  --node-pool $NODEPOOL --no-enable-autoscaling
```

One-time check: the node pool itself must carry
`ate.dev/substrate-version=$OLD`, or nodes GKE creates later arrive
unlabeled and nothing schedules to them; if it is missing, stamp it
the same way step 7 does, with the OLD value.

```
gcloud container node-pools describe $NODEPOOL --cluster $CLUSTER --zone $ZONE \
  --format='get(config.labels)' # copy the value
gcloud container node-pools update $NODEPOOL --cluster $CLUSTER --zone $ZONE \
  --node-labels=<every existing user label>,ate.dev/substrate-version=$OLD
```

### 2. Upgrade ate-controller

The controller crosses first. Check out the new release and deploy:

```bash
git checkout <new release tag>
go run ./cmd/ate-setup deploy controller
```

This applies the new CRDs and rolls ate-controller; nothing else
moves. Changes to how the controller renders worker pods are gated
behind WorkerPool fields by convention, so the new controller keeps
rendering the serving pools as they are. A release that breaks that
convention says so in its notes; expect the pools' Deployments to
roll once here, which suspends every actor through the worker
eviction path (no state is lost) and lets them resume on demand.

### 3. Stage the new dataplane

The dataplane roll starts with the new atelet DaemonSet, from the
same checkout:

```bash
go run ./cmd/ate-setup deploy atelet
```

It lands next to the old one and has zero pods until step 5 flips a
node; existing nodes keep their labels, since the install only labels
unlabeled nodes. (Caveat: with an OTLP endpoint override configured
it pod-restarts the running atelet DaemonSets; worker pods are
untouched.)

Then build and push the new worker images, and note the
`<binary>: <ref>` line for your pool's sandbox class. This one still
goes through the shell installer; ate-setup has no equivalent yet:

```bash
./hack/install-ate.sh --publish-worker-images
```

Read `$NEW` off the cluster, not off `git describe` (a stray edit in
the checkout makes describe emit a different `-dirty` value than the
deploy stamped, and the mismatch surfaces only later, as pods that
never schedule):

```bash
kubectl get ds -n ate-system -l app=atelet \
  -o jsonpath='{range .items[*]}{.metadata.labels.ate\.dev/substrate-version}{"\n"}{end}'
```

Two values print; the one that is not `$OLD` is `$NEW`. Confirm every
node still shows `$OLD` and the new DaemonSet has 0 pods.

Then set up the drain helper. Draining a worker is one `DrainWorker`
RPC on the installed ate-api-server; no released CLI wraps it yet, so
the roll uses [grpcurl](https://github.com/fullstorydev/grpcurl) (the
server has gRPC reflection):

```bash
kubectl -n ate-system port-forward svc/api 8443:443 &

kubectl get clustertrustbundles -l podcert.ate.dev/canarying=live \
  -o jsonpath='{range .items[?(@.spec.signerName=="servicedns.podcert.ate.dev/identity")]}{.spec.trustBundle}{end}' \
  > ate-ca.pem

# Tokens expire after an hour; rerun on auth errors mid-roll.
TOKEN=$(kubectl -n ate-system create token ate-client \
  --audience=api.ate-system.svc --duration=1h)

# Drain one worker. Idempotent; on ABORTED (concurrent write), rerun.
drain_worker() {
  grpcurl -cacert ate-ca.pem -authority api.ate-system.svc \
    -H "authorization: Bearer ${TOKEN}" \
    -d '{"worker": {"name": "'"$1"'"}}' \
    127.0.0.1:8443 ateapi.Control/DrainWorker
}

# Workers on a node (the table has no node column; read the JSON).
workers_on_node() {
  kubectl ate get workers -o json \
    | jq -r '.workers[] | select(.nodeName == "'"$1"'") | .metadata.name'
}
```

### 4. Create the new pool

Repeat this step for every serving pool the preflight listed.

Clone the old pool's live spec under a new name, overriding only the
worker image and the version pin. Everything else (metadata labels,
replicas, resources, sandbox fields) is inherited from the dump, and
the inherited labels are what make the clone work: the scheduler
matches actors to workers by the pool's `metadata.labels` (not
`spec.template.labels`), so a clone that keeps them serves the same
actors.

```bash
kubectl -n $NS get workerpool $OLD_POOL -o json | jq '
  {apiVersion, kind,
   metadata: {name: "'"$NEW_POOL"'", namespace: .metadata.namespace,
              labels: .metadata.labels},
   spec: (.spec + {workerImage: "<the ateom ref printed in step 3>"})}
  | .spec.template.nodeSelector["ate.dev/substrate-version"] = "'"$NEW"'"
' | kubectl apply -f -
```

For a micro-VM pool, also override `.spec.sandboxConfigName` with the
new SandboxConfig from ground rule 6. Do not shrink
`spec.template.resources.limits` while cloning: a worker only accepts
an actor whose limits fit under them.

Verify:

```bash
# The pin is byte-identical to $NEW as read in step 3:
kubectl -n $NS get workerpool $NEW_POOL \
  -o jsonpath='{.spec.template.nodeSelector.ate\.dev/substrate-version}'

# One Deployment per pool; new-pool pods all Pending (no node carries
# the new label yet).
kubectl -n $NS get deploy -l ate.dev/worker-pool
kubectl -n $NS get pods -l ate.dev/worker-pool=$NEW_POOL
```

While both pools serve, placement between them is random; the roll
converges because step 5 takes old workers out of service node by
node, not because the scheduler favors the new pool.

### 5. Roll each node

Repeat for every node, one at a time.

**a. Drain the node's workers.** Bound actors keep running; draining
only stops new placements.

```bash
for w in $(workers_on_node $NODE); do drain_worker "$w"; done
```

**b. Wait for the node to empty.** Owners suspend at their own pace
(`kubectl ate suspend actor <name> -a <atespace>`); the node is empty
when no worker on it carries an assignment. Do not read the STATUS
column for this (it derives from the assignment, not the drain):

```bash
kubectl ate get workers -o json | jq -r \
  '.workers[] | select(.nodeName == "'"$NODE"'")
   | .metadata.name + " assigned: "
     + (.status.assignment.actor.name // "<none>")'
```

This wait has to catch four things:

- Every actor on the node reaches ACTOR_STATE_SUSPENDED in
  `kubectl ate get actors -A`, not merely idle: suspend is what
  uploads the durable snapshot and releases the worker.
- The drain can crash an actor that was mid-resume
  (ACTOR_STATE_CRASHED, terminal). Do not wait on a crashed actor; it
  can only be deleted and recreated.
- A paused actor that slipped past the preflight appears on no worker
  row. Find any pinned to this node and suspend them, or they are
  stranded when the label flips:

  ```bash
  kubectl ate get actors -A -o json | jq -r \
    '.actors[]
     | select(.status.localSnapshotInfo.nodeVmsWithLocalSnapshots // [] | index("'"$NODE"'"))
     | .metadata.atespace + "/" + .metadata.name'
  ```

- A resume already in flight when you drained can still land on a
  draining worker; suspend the newcomer too.

Displaced actors resume on demand onto any free matching worker,
possibly an old-pool worker on an unrolled node; such an actor is
displaced again when that node's turn comes.

**c. Final pass, immediately before the flip.** A crashed old-pool
pod can reschedule here and register as a fresh undrained worker, and
a late resume can land after your last check. Rerun the drain and
recheck that every worker shows `<none>`; if anything is assigned, go
back to step b.

**d. Flip the label.** The old atelet pod leaves on its own; the new
one starts.

```bash
kubectl label node $NODE ate.dev/substrate-version=$NEW --overwrite
```

**e. Delete the node's old-pool worker pods.** They are empty per
step c but still Ready, holding capacity the new pool needs here;
their Deployment cannot reschedule them onto this node anymore.
Repeat per pool if the node hosts several.

```bash
kubectl -n $NS delete pod -l ate.dev/worker-pool=$OLD_POOL \
  --field-selector spec.nodeName=$NODE
```

**f. Wait before taking the next node**: this node's new atelet pod
Ready, the new-pool pods on it Ready (new-pool pods Pending elsewhere
are expected mid-roll), and at least one FREE worker in
`kubectl ate get workers` as a landing spot for the actors the next
drain displaces. Nothing queues a resume waiting for capacity (with
no free matching worker it parks in the router, retries, and
eventually fails), so never drain faster than replacement capacity
comes up. On the first node, also confirm one displaced actor
actually resumes and runs on a new-pool worker (ATEOM POD in
`kubectl ate get actors -A` names the pod, and pods are named after
their pool) before rolling the rest of the fleet.

When every node is done, sweep for stragglers: nodes that joined
mid-roll arrive at the node pool's label version, so
`kubectl get nodes -L ate.dev/substrate-version` may still show
`$OLD` entries. Each gets the step 5 treatment; a node with no
workers just gets its label flipped, and actors from a node GKE
recreated mid-roll were suspended through the eviction path and
resume elsewhere on demand. Repeat until the node
list is uniform and every assigned actor shows a new-pool pod in
ATEOM POD.

### 6. Upgrade the rest of the control plane

Every actor now runs on, or is suspended off, the new dataplane, and
ate-controller crossed in step 2. Finish the control plane from the
same checkout:

```bash
go run ./cmd/ate-setup deploy ate-system
```

This rolls ate-api-server and atenet to the new build (ate-controller
and the atelet DaemonSets are already there) and re-applies the
default gVisor SandboxConfig at the new build, so cold boots fetch
the new binaries from here on. The step 3 port-forward dies when the
API server rolls; restart it and mint a fresh token if you still need
the drain helper.

Ground rule 4 ends here: the fleet can go back to normal operation.

### 7. Move the pool label, restore the autoscaler (GKE)

Move the node pool label so nodes born later start at the new
version, then restore the autoscaler from the config step 1 saved:

```bash
# --node-labels REPLACES the pool's full user label set: list the
# current labels first and carry them all over. Never include
# GKE-managed keys (gke.io, kubernetes.io, k8s.io, cloud.google.com
# domains): the update API rejects them.
gcloud container node-pools describe $NODEPOOL --cluster $CLUSTER --zone $ZONE \
  --format='get(config.labels)'
gcloud container node-pools update $NODEPOOL --cluster $CLUSTER --zone $ZONE \
  --node-labels=<every existing user label>,ate.dev/substrate-version=$NEW

gcloud container clusters update $CLUSTER --zone $ZONE --node-pool $NODEPOOL \
  --enable-autoscaling --min-nodes <min> --max-nodes <max>
```

If your pool used total limits or a location policy, restore those
with `--total-min-nodes`, `--total-max-nodes`, and
`--location-policy`; check the saved JSON.

## Rollback

The roll never edits or deletes the old objects: the old WorkerPool
is intact, its displaced pods are the Pending reserve, and the old
atelet DaemonSet is still installed. Rolling nodes back is label
flips; the control plane needs one deploy from the old release. Check
out the old release tag and run `go run ./cmd/ate-setup deploy
controller` if you got no further than step 5, or `go run
./cmd/ate-setup deploy ate-system` if step 6 ran (that also restores
the old SandboxConfig). If step 7 ran, move the pool label back
first.

One scope limit: an actor that suspended on a new-pool worker now has
a snapshot written by the new build, and restoring it through the old
build is a cross-version pairing with no stated guarantee. List
snapshots created since the upgrade started
(`kubectl ate get actor-snapshots -A`, AGE column), treat their
actors as at-risk, and test one on the first rolled-back node. The
rollback promise weakens the longer the new version has served.

To roll a node back, run step 5 with the sides swapped: drain, wait
with the same suspend discipline and final pass, flip the label back
to `$OLD`, and delete the node's NEW-pool pods. To abandon the
upgrade entirely: roll every flipped node back, confirm no actor is
assigned to a new-pool worker, then delete the new objects:

```bash
kubectl -n $NS delete workerpool $NEW_POOL
kubectl delete daemonset -n ate-system -l app=atelet,ate.dev/substrate-version=$NEW
```

Retiring the old pool (below) is deliberately separate: as long as
the old objects exist, rollback is one label flip per node.

## Retire the old pool

After the new version has soaked, reclaim the reserve. Save the old
pool's spec first (its `workerImage` ref is by digest and stays
pullable, so the saved file is the last-resort re-create path), then
check the guards; deleting the old objects is what ends the rollback
option.

```bash
kubectl -n $NS get workerpool $OLD_POOL -o yaml > old-pool-backup.yaml

# Guards: no node still at $OLD; no old-pool pod Running (Pending is
# expected); no actor assigned to an old-pool worker.
kubectl get nodes -l ate.dev/substrate-version=$OLD
kubectl -n $NS get pods -l ate.dev/worker-pool=$OLD_POOL
kubectl ate get workers

# Retire the old pool (its Deployment and pods go with it) and the
# old atelet DaemonSet.
kubectl -n $NS delete workerpool $OLD_POOL
kubectl delete daemonset -n ate-system -l app=atelet,ate.dev/substrate-version=$OLD
```

The new pool keeps its name; names mean nothing to placement, so
`counter-v2` serves indefinitely and the next upgrade clones it to
`counter-v3`.

## If something goes wrong

- Every step is idempotent (rerun the command that failed), with one
  exception: there is no undrain. If you drained the wrong node,
  verify its workers are empty (suspending as needed), delete their
  pods, and let the Deployment's replacements register fresh.
- `kubectl get nodes -L ate.dev/substrate-version`,
  `kubectl -n $NS get deploy -l ate.dev/worker-pool`, and
  `kubectl ate get workers` are the whole truth of where the roll
  stands; there is no hidden state.
- 503s during the roll usually mean a resume found no free matching
  worker: you drained ahead of replacement capacity. Stop draining
  and wait for FREE new-pool workers.
- Do not re-run a demo deploy mid-roll: it applies the WorkerPool
  manifest at the checked-out build over the serving pool, the
  in-place edit ground rule 3 forbids. Developing against a
  long-lived cluster is a different workflow from upgrading it: pin
  the build version (see hack/ate-dev-env.sh.example) so redeploys
  converge in place.
