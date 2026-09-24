# Upgrade runbook

This runbook upgrades a running Agent Substrate install to a newer release in the same release window, meaning the same `v0.x`: for example, from v0.2.0 to v0.2.1. Between windows, for example from v0.1.x to v0.2.x, reinstall instead. 

> [TODO]  Add the version skew policy.


| Step | What changes | What running actors see | How long |
|---|---|---|---|
| 1 | CRDs | Nothing | Seconds |
| 2 | ate-controller | Usually nothing; see step 2 | A minute, or as long as step 4 if it replaces workers |
| 3 | atelet, one node at a time | While a node's atelet restarts, suspend, pause and resume requests for actors on that node can fail | Seconds to about 6 minutes per node |
| 4 | workers | `SIGTERM`, then a 30-minute window to be suspended before `SIGKILL` | Minutes to hours per pool |
| 5 | ate-api-server, then atenet | API calls and long HTTP requests can be cut once | A few minutes |

ate-controller manages the WorkerPools, so it goes first. atelet and the workers go before ate-api-server and atenet, so that when the API server changes, every node already understands requests from either version. Rollback is the same list in reverse, from the old release. An actor that crashes along the way goes back to its last snapshot with one revert call.

The system upgrade does not change sandbox runtimes, the database engine, Kubernetes version or the node OS version.

## Before you start

The commands are for bash. Check that `kubectl config current-context` prints the cluster you are upgrading.

Check out both releases. ate-setup runs from the new checkout. The old checkout is for rollback, and its `kubectl ate` is the one to use until step 5 is done.

```bash
OLD_RELEASE=<installed release, for example v0.2.0>
NEW_RELEASE=<new release, for example v0.2.1>
mkdir ~/ate-upgrade && cd ~/ate-upgrade
git clone --branch $OLD_RELEASE https://github.com/agent-substrate/substrate.git old
git clone --branch $NEW_RELEASE https://github.com/agent-substrate/substrate.git new
(cd old && go install ./cmd/kubectl-ate)
```

Prebuilt images show the installed release in their tag, in `kubectl -n ate-system get deploy,ds -o wide`. For images built from source, `kubectl -n ate-system exec deploy/ate-api-server -c ate-api-server -- /ko-app/ateapi --version` prints it.

### Use the install's settings

ate-setup does not store the settings you installed with. A command run without one of them puts that part back to its default, such as a different database or atenet proxy. Write the same `export` lines you installed with into `~/ate-upgrade/settings.sh`, including any from a `.ate-dev-env.sh`, and add:

- **Images:** the new release's `ATE_IMAGE_REPO` and `ATE_IMAGE_TAG`, or `KO_DOCKER_REPO` to build from source.
- **kind:** `ATE_INSTALL_KIND=true`.
- **Wait time:** `ATE_INSTALL_ROLLOUT_TIMEOUT=1h`. The 60-second default is too short for step 3.

Leave out `ATE_OTLP_ENDPOINT` until the end of step 5. ate-setup does not keep a custom endpoint, and with it set, the later steps restart other components and replace every worker again. Also leave out `ATE_API_POSTGRES_CLOUDSQL_INSTANCE`: ate-setup keeps Cloud SQL by itself, and an empty value removes it.

Run this now, and in every new shell:

```bash
cd ~/ate-upgrade/new
source ~/ate-upgrade/settings.sh
```

Then save each pool's image for rollback, and the actors that are already CRASHED:

```bash
kubectl get workerpools -A -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,IMAGE:.spec.workerImage' > ~/ate-upgrade/before-workerpools.txt
kubectl ate get actors -A | awk '$4=="ACTOR_STATE_CRASHED" {print $1, $2}' > ~/ate-upgrade/before-crashed.txt
```

> [!WARNING]
> Do not run `ate-setup deploy ate-system`, `deploy demo ...`, `deploy benchmarks`, `delete ate-system` or `delete all` during the upgrade. They update components out of order, move pools to the checkout's worker image, or delete every WorkerPool.

## Upgrade

### Step 1. Apply the new CRDs

```bash
kubectl apply --server-side --field-manager=ate-setup --force-conflicts -f manifests/ate-install/generated
```

This updates the three CRDs and ate-controller's RBAC. The flags make kubectl apply the files the way ate-setup does, so step 2 does not conflict with this change.

- **Done:** five lines ending in `serverside-applied`.
- **Actors:** nothing changes. The step is safe to rerun. Go on to step 2 right away.

### Step 2. Upgrade ate-controller

> [!WARNING]
> If this release changes the worker pod template, or the install set `ATE_OTLP_ENDPOINT`, step 2 replaces every worker on its old image, like step 4.

```bash
go run ./cmd/ate-setup deploy ate-controller
```

It applies the CRDs again, deploys the new ate-controller and waits for it.

- **Done:** the command exits 0, and `kubectl -n ate-system get pods -l app=ate-controller` shows one pod, Running, with RESTARTS 0. Ready only means the container started, so also run `kubectl -n ate-system logs deploy/ate-controller | grep -E 'atecontroller starting|Starting workers|Reconciler error'`: it should show the first two and not the third.
- **Stuck:** the new pod is Pending or in ImagePullBackOff: the old controller still runs. The new pod is in CrashLoopBackOff or its RESTARTS keeps rising: the old controller is already gone, and nothing manages the WorkerPools. Either way, do not go on; [roll back step 2](#roll-back).

A minute after the new pod starts, check whether it is replacing workers:

```bash
kubectl get deploy -A -l ate.dev/worker-pool -o custom-columns='NAMESPACE:.metadata.namespace,POOL:.metadata.name,DESIRED:.spec.replicas,UPDATED:.status.updatedReplicas,AVAILABLE:.status.availableReplicas,DRAINING:.status.terminatingReplicas,ROLLOUT_PAUSED:.spec.paused'
```

This is the pool list. A pool is rolling while UPDATED or AVAILABLE is below DESIRED, or DRAINING is above 0; `<none>` means 0 for UPDATED and AVAILABLE. Wait until no pool is rolling, then go to step 3. This can take hours; step 4 says what is normal.

### Step 3. Upgrade atelet

> [!WARNING]
> Do not run step 3 while many actors are being suspended, paused or resumed, including automatic resumes when requests arrive, and do not create ActorTemplates during it. While a node's atelet restarts, those calls on that node can fail and leave the actor CRASHED, or stuck in SUSPENDING, PAUSING or RESUMING.

> [!NOTE]
> TODO: Shrink this warning if the API server retries these calls instead of crashing the actor.

```bash
go run ./cmd/ate-setup deploy atelet
kubectl -n ate-system rollout status ds/atelet
```

If ate-setup fails with `waiting for daemonset/atelet in ate-system: ...`, only its wait timed out, and the second command keeps watching. If it fails with any other error, it did not change atelet: fix the error and run step 3 again.

- **Progress:** `Waiting for daemon set "atelet" rollout to finish: 2 out of 5 new pods have been updated...`, then `... 4 of 5 updated pods are available...`.
- **Done:** `daemon set "atelet" successfully rolled out`.
- **Stuck:** the `N out of M` count does not move for more than about 6 minutes. Run `kubectl -n ate-system get pods -l app=atelet -o wide`. A pod that is not Running leaves its node without atelet. If it is in ImagePullBackOff or CrashLoopBackOff, [roll back step 3](#roll-back). If it is stuck in ContainerCreating, check podcertificate-controller.
- **Actors:** running actors keep running and keep serving HTTP. A resume that reaches a restarting node can return HTTP 500 `error resuming actor <atespace>/<name>`; a retry usually works.

When it is done, [recover CRASHED actors and actors stuck mid-call](#recover-crashed-actors).

### Step 4. Move each WorkerPool to the new worker image

> [!WARNING]
> Do not start step 4 until step 3 is done: new workers need the new atelet. Do not start it while any actor is PAUSED. This must print nothing:
>
> ```bash
> kubectl ate get actors -A | awk '$4=="ACTOR_STATE_PAUSED" {print $1, $2}'
> ```
>
> Do not edit the pool's Deployment with `kubectl set image`, `kubectl rollout undo` or `kubectl edit`: ate-controller puts it back. Do not `kubectl apply -f` a whole WorkerPool: that also resets `spec.replicas` on a pool an autoscaler sizes. If a GitOps tool syncs your WorkerPools, turn off its automatic sync for them first, or it moves the pools back to the old image.

List the pools. Each needs the new image for its sandbox CLASS:

```bash
kubectl get workerpools -A -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,CLASS:.spec.sandboxClass,IMAGE:.spec.workerImage'
```

If you build from source, build and push both worker images once. This touches nothing in the cluster and ends by printing `ateom-gvisor: <ref>` and `ateom-microvm: <ref>`:

```bash
go run ./cmd/ate-setup publish worker-images | tee ~/ate-upgrade/worker-images.txt
```

> [!NOTE]
> TODO: Document where release images are published and their tag format. Open source releases do not publish images yet.

Then move the pools one at a time. For each pool:

1. Set the pool:

   ```bash
   NS=<pool namespace>
   POOL=<pool name>
   CLASS=$(kubectl -n $NS get workerpool $POOL -o jsonpath='{.spec.sandboxClass}')
   ```

2. Set its new image. From source, copy the ref after `ateom-$CLASS: ` in `~/ate-upgrade/worker-images.txt`:

   ```bash
   NEW_IMAGE=<ref>
   ```

   With prebuilt images, look it up pinned by digest. Without crane, get the digest with `gcloud artifacts docker images describe <image> --format='value(image_summary.digest)'`.

   ```bash
   NEW_IMAGE=$ATE_IMAGE_REPO/ateom-$CLASS:$ATE_IMAGE_TAG@$(crane digest $ATE_IMAGE_REPO/ateom-$CLASS:$ATE_IMAGE_TAG)
   ```

3. Check that `echo "$NS/$POOL $CLASS $NEW_IMAGE"` shows `ateom-` plus the pool's CLASS and a full `@sha256:` digest. A bad image still removes the pool's first old worker.
4. Patch the pool, wait until ate-controller moves its Deployment to the new image, and watch the roll:

   ```bash
   kubectl -n $NS patch workerpool $POOL --type merge -p "{\"spec\":{\"workerImage\":\"$NEW_IMAGE\"}}"
   kubectl -n $NS wait deployment/$POOL --for=jsonpath='{.spec.template.spec.containers[0].image}'="$NEW_IMAGE" --timeout=2m
   kubectl -n $NS rollout status deployment/$POOL
   ```

5. Go to the next pool once rollout status prints `successfully rolled out`. The times of pools done one after another add up. Pools patched together roll in parallel and interrupt more actors at once.

What to expect:

- **Progress:** rollout status prints, in order:

  ```
  Waiting for deployment "<pool>" rollout to finish: 2 out of 10 new replicas have been updated...
  Waiting for deployment "<pool>" rollout to finish: 1 old replicas are pending termination...
  Waiting for deployment "<pool>" rollout to finish: 9 of 10 updated replicas are available...
  deployment "<pool>" successfully rolled out
  ```

  "Updated" includes workers still starting. Ctrl-C stops only the watch, not the roll.
- **Done:** `successfully rolled out` does not count old workers that are still draining, and their actors may still be inside their 30 minutes. Step 4 is done when every pool shows UPDATED and AVAILABLE equal to DESIRED, and DRAINING 0, in the [pool list](#step-2-upgrade-ate-controller). DRAINING `<none>` means your cluster does not report the count (Kubernetes 1.35 and later do); then wait until `kubectl ate get workers` shows no `WORKER_STATE_DRAINING`.
- **Pace:** how many actors are interrupted at once depends on free node room, not on the batch size. A batch is 10% of the pool's workers, rounded down and at least one, so pools under 20 workers replace one worker at a time. At most one batch is not ready at a time. A 1-worker pool has no ready worker while its worker is replaced.
  - While a pool rolls, up to one batch of its workers takes no new actors. If fewer of its workers are idle than that, resumes fail with `no free workers available` until the batch is ready. To avoid this, raise the pool's replicas before you patch it.
  - If the nodes have room, replacements start at once, and most old workers in the pool get `SIGTERM` within minutes.
  - If the nodes are full, each replacement waits for an old worker to exit: up to 30 minutes plus a pod start per batch. That is up to 30 minutes per worker for pools under 20 workers, and 5 to 8 hours for larger pools. A batch ends early once its actors are suspended, and idle workers exit at once.
- **Stuck, or normal?**
  - On full nodes, new pods Pending next to old pods Terminating, with rollout status on one line for up to about 30 minutes, are normal. With an autoscaler, pods Pending for a few minutes while a node is added are normal.
  - DESIRED minus READY in `kubectl get workerpools -A` stays above one batch for more than a few minutes, or rollout status fails with `error: deployment "<pool>" exceeded its progress deadline` (80 minutes without progress; the roll goes on). Look for Pending pods (no capacity) or ImagePullBackOff (bad image) with `kubectl -n $NS get pods -l ate.dev/worker-pool=$POOL` and fix the cause. Until the roll moves again, rollout status repeats the error at once.
  - A bad image stops the roll after the first batch. [Roll back that pool](#roll-back).
  - DRAINING above 0 for more than 60 minutes, the longest a worker pod takes to exit, means a pod is stuck Terminating, usually on an unreachable node. Find it with `kubectl -n $NS get pods -l ate.dev/worker-pool=$POOL`.
- **Actors:** each gets `SIGTERM` and 30 minutes to be suspended, as described under eviction in the [API guide](api-guide.md). A resume in flight on a worker when its batch starts can crash that actor at once. The actor had not started running there, so a revert loses nothing. The metric `ate.actor.crashes` with `ate.actor.operation.name` `unknown` counts actors that were not suspended in time.

When every pool is done, put the new `workerImage` in any files you apply WorkerPools from, turn GitOps sync back on, and [recover CRASHED actors](#recover-crashed-actors).

### Step 5. Upgrade ate-api-server, then atenet

> [!WARNING]
> Do not start step 5 until step 4 is done, and the [pool list](#step-2-upgrade-ate-controller) shows ROLLOUT_PAUSED `<none>` for every pool. Do not use `deploy ate-system` here. `deploy apiserver` rewrites the API server's database settings from your shell every time, so it runs after a fresh `source` below.

First the API server. The last command lists your actors through it:

```bash
cd ~/ate-upgrade/new
source ~/ate-upgrade/settings.sh
go run ./cmd/ate-setup deploy apiserver
kubectl -n ate-system rollout status deploy/ate-api-server
kubectl ate get actors -A
```

- **Done:** rollout status prints `deployment "ate-api-server" successfully rolled out`, and the list shows your actors.
- **Stuck:** an empty list means the API server points at the wrong database: add the install's `ATE_API_POSTGRES_CONNECTION_STRING` and `ATE_API_POSTGRES_SCHEMA` to `settings.sh` and run the block again. A new pod that never becomes ready makes `deploy apiserver` fail after about 10 minutes with `exceeded its progress deadline`, while the old pods keep serving. Check `kubectl -n ate-system get pods -l app=ate-api-server` and the new pod's logs: a database error has the same fix as an empty list, and ImagePullBackOff means wrong image settings. For anything else, [roll back step 5](#roll-back).

Then atenet:

```bash
go run ./cmd/ate-setup deploy atenet
kubectl -n ate-system rollout status deploy/atenet-router
kubectl -n ate-system rollout status deploy/atenet-egress
```

- **Done:** both print `successfully rolled out`.
- **Stuck:** a new pod stays Pending, in ImagePullBackOff or CrashLoopBackOff; the old pod keeps serving. Check `kubectl -n ate-system get pods` and [roll back step 5](#roll-back) if you cannot fix it.
- **API clients:** a suspend or resume running when its API server pod stops can be cut off and left mid-call, so harnesses should retry API errors during this step. If a `kubectl ate` command fails once, run it again. A `kubectl port-forward` you opened yourself breaks when its pod goes.
- **Actors:** when the router restarts, an HTTP request that takes longer than about 10 seconds can be cut once, and new connections fail for a few seconds. Outbound connections through the egress gateway are reset once.

Then finish:

1. Install the new `kubectl ate` with `go install ./cmd/kubectl-ate`. Actor owners can now use API fields and commands new in this release.
2. If the install set `ATE_OTLP_ENDPOINT`, restore it now. Suspend any PAUSED actors first. Add the `export` line to `settings.sh`, source it, and run `go run ./cmd/ate-setup deploy atenet`. This restarts atelet, so the step 3 warning applies, and replaces every worker again: wait as in step 4, until DRAINING is 0. If you roll back later, remove the line from `settings.sh` first.
3. [Recover CRASHED actors](#recover-crashed-actors).

The upgrade is done when step 5 is done and no new actor stays CRASHED. Tell actor owners they can pause actors again. Keep `~/ate-upgrade` while you might still roll back, then delete it: `settings.sh` can hold passwords.

> [!NOTE]
> TODO: podcertificate-controller is not part of the five steps. Say how to upgrade it when a release changes it.

## Something's wrong, or I need to roll back

Stopping between steps is safe.

### Symptoms

| You see | Meaning | Do |
|---|---|---|
| `waiting for daemonset/atelet in ate-system: ... (last status: 3/9 nodes updated)` | Only ate-setup's wait timed out. | `kubectl -n ate-system rollout status ds/atelet` |
| An atelet pod in ImagePullBackOff or CrashLoopBackOff | That node has no atelet, and step 3 stopped there. | [Roll back step 3](#roll-back). |
| ate-controller pod restarting after step 2 | Nothing manages the WorkerPools. | [Roll back step 2](#roll-back). |
| `exceeded its progress deadline` for a pool, or worker pods Pending or in ImagePullBackOff | Step 4 is slow or blocked. | See **Stuck, or normal?** in [step 4](#step-4-move-each-workerpool-to-the-new-worker-image). |
| HTTP 503 whose body mentions `ACTOR_STATE_CRASHED` | The actor crashed. | [Revert it](#recover-crashed-actors). |
| HTTP 503 `no free workers available` | During step 4 the pool is short by up to one batch. | Wait, or add capacity. If the actor is PAUSED, suspend it, then resume it. |
| HTTP 503 `another operation is in progress for this actor` | A suspend is running. | Wait. |
| HTTP 503 `actor <atespace>/<name> unavailable`, with no detail | ate-api-server restarting (step 5). | Retry. |
| HTTP 500 `error resuming actor <atespace>/<name>` | atelet restarting (step 3). | Retry. |
| InvalidArgument or Unimplemented from a `kubectl ate` command or API call | It is new in this release, and the old API server still serves. | Finish step 5 first. |
| After step 5, the atenet proxy changed or credentials are no longer injected | Install settings were missing from `settings.sh`. | Add them, source it, run `deploy atenet` again. |

### Recover CRASHED actors

A CRASHED actor needs one revert. It returns the actor to SUSPENDED at its last snapshot, and the next call resumes it. Work since that snapshot is lost, and external volumes are not rewound.

This lists actors that became CRASHED since you started, each as `> <atespace> <name>`. An actor can show CRASHED for a moment while its suspend finishes, so run it again after a minute, and revert only actors that stay on the list.

```bash
kubectl ate get actors -A | awk '$4=="ACTOR_STATE_CRASHED" {print $1, $2}' | diff ~/ate-upgrade/before-crashed.txt -
```

For each one:

```bash
ATESPACE=<atespace>
ACTOR=<name>
kubectl ate revert actor $ACTOR -a $ATESPACE
```

A revert while a suspend is still finishing fails with `code = Aborted desc = another operation is in progress for this actor` and changes nothing; the actor turns SUSPENDED by itself.

After step 3, also list actors that a failed call left halfway, each as `<atespace> <name> <state>`. Actors in the middle of a normal call show up too, so run it again after a few minutes:

```bash
kubectl ate get actors -A | awk '$4 ~ /^ACTOR_STATE_(SUSPENDING|PAUSING|RESUMING)$/ {print $1, $2, $4}'
```

For each actor still listed, run its call again: `kubectl ate suspend actor` for SUSPENDING, `pause actor` for PAUSING (then suspend it, because step 4 must not start with a paused actor), or `resume actor` for RESUMING, each with `$ACTOR -a $ATESPACE`.

### Stop a worker roll partway

To stop step 4 between pools, stop patching; unpatched pools do not roll. To stop a pool that is rolling, pause its Deployment. This is the one Deployment change ate-controller leaves in place.

```bash
kubectl -n $NS rollout pause deployment/$POOL
```

No more old workers are removed. Workers already removed keep shutting down, and their actors still get the rest of their 30 minutes. The pool's size is not frozen: a change to its replicas, by you or an autoscaler, still adds or removes workers. While the pool is paused, rollout status never finishes; press Ctrl-C. To continue, run `kubectl -n $NS rollout resume deployment/$POOL` and watch as in step 4.

> [!WARNING]
> Do not leave a pool paused, and do not start step 5 while one is. A paused pool ignores every later change to its workers, including the next upgrade. The [pool list](#step-2-upgrade-ate-controller) shows it as ROLLOUT_PAUSED `true`.

### Roll back

To go back to the old release, roll back every step you began, finished or not, starting with the last. To fix one pool, roll back only that pool. Rollback runs from the old checkout. Run this block now, and in every new shell during the rollback; the last line is for prebuilt images only:

```bash
cd ~/ate-upgrade/old
source ~/ate-upgrade/settings.sh
export ATE_IMAGE_TAG=<old release's image tag>
```

Do not edit `settings.sh` for the rollback: going forward again needs the new tag.

> [!WARNING]
> Do not roll back step 3 before every pool is back on its old image, with DRAINING 0. A new worker with an old atelet is not supported.

- **Step 5:** tell actor owners to stop using API fields new in this release, because the old API server rejects them. Run `go run ./cmd/ate-setup deploy atenet`, then `go run ./cmd/ate-setup deploy apiserver`, then reinstall the old `kubectl ate` with `go install ./cmd/kubectl-ate`. The database schema stays new; the old API server drops fields it does not know when it rewrites a record.
- **Step 4:** suspend any PAUSED actors again. Then set each patched pool back to its IMAGE in `~/ate-upgrade/before-workerpools.txt`, with the exact string; another reference to the same image counts as a new image and replaces every worker.

  ```bash
  NS=<pool namespace>
  POOL=<pool name>
  OLD_IMAGE=<IMAGE for this pool in before-workerpools.txt>
  kubectl -n $NS patch workerpool $POOL --type merge -p "{\"spec\":{\"workerImage\":\"$OLD_IMAGE\"}}"
  ```

  If you paused the pool, resume it now with `kubectl -n $NS rollout resume deployment/$POOL`. Then watch:

  ```bash
  kubectl -n $NS rollout status deployment/$POOL
  ```

  Partway through a roll, only workers already on the new image are replaced. After a finished roll, every worker is replaced again, with another round of `SIGTERM`. Wait as in step 4, until DRAINING is 0. If an actor turns CRASHED again right after each resume, the old workers cannot restore the snapshot a new worker wrote, and a revert does not help: move that pool forward to the new image again. If you put the new image in your WorkerPool files, put the old one back before turning GitOps sync on.
- **Step 3:** once every pool is back, run `go run ./cmd/ate-setup deploy atelet`. The step 3 warning applies.
- **Step 1 or 2:** run `go run ./cmd/ate-setup deploy ate-controller`. It restores the old CRDs, RBAC and controller. If the release changed the worker pod template, every pool rolls again: suspend any PAUSED actors first, and wait as in step 4. Fields the new release added to WorkerPools, SandboxConfigs or CSIDriverConfigs disappear; remove them from your manifests first, or `kubectl apply` fails. A value only the new release accepts, such as a new `sandboxClass`, stays but is not understood; change it first. If the new release added an API version to a CRD, you cannot roll back past step 1; reinstall.

> [!NOTE]
> TODO: The upgrade end-to-end test does not cover rollback.
