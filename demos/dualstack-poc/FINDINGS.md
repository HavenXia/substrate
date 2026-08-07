# Dual-live POC findings

Two full substrate stacks (0.1.0 blue, 0.2.0 green) ran side by side in one
`ate-system` on GKE `substrate-poc`; one `Substrate` CR installed 0.1.0 and a
one-line version bump drove GreenUp → CommitPaused → Flip → Migrate → Teardown
to Ready-at-0.2.0. Five actor stories rode along. This file is the evidence
the meeting asked for: every place the two stacks turned out to share
something, every fence we had to add, and the measured cross-stack traffic.

Branch `dualstack-poc` (local): basis commits + build wave + fixes.
Run transcripts: `RESULTS.md`. Raw dispatcher decisions: `dispatcher-run*.jsonl`.

## What passed

All five stories, over a real upgrade on GKE:

- (a) cooperative blue actor: served through the window, got the operator's
  SIGTERM, finished its work item, harness suspended it (blue→blue) and the
  next resume landed green with RAM intact. (Run 1 hit the rules-propagation
  race below; retry recovered it; fixed for run 2.)
- (b) actor created after the flip was born green.
- (c) suspended blue actor woken by router traffic resumed green; its
  blue-written GCS snapshot restored cross-version; counter continued 3→4→5.
- (d) stubborn blue actor ignored SIGTERM; at the deadline the operator
  deleted its blue worker pod; the blue syncer marked it CRASHED
  (ReasonWorkerPodGone) and freed the worker; teardown was not blocked. No
  recovery attempted (out of scope).
- (e) paused blue actor was committed in place pre-flip (#791: blue atelet
  uploaded the local pause snapshot, no VM boot), later resumed green with
  RAM intact.

Timings (run 2, clean): install-to-Ready 58s; bump-to-Ready-at-0.2 2m14s
(grace 90s dominates); GreenUp+CommitPaused+Flip ≈ 36s; one 15.3s data-plane
gap during the flip window (door flip + dispatcher bounce), then continuous.
Run 2 needed zero manual interventions; run details in RESULTS.md.

## Basis changes (below both versions)

1. Binary proto store for all valkey records (was strict protojson —
   unknown-field poisoning was live-proven in July). Unknown fields now
   round-trip through the old side's read-modify-writes.
2. #791 suspend-a-paused-actor: new atelet `UploadCheckpoint` RPC +
   from-pause path in the suspend workflow (api dials the atelet BY NODE —
   paused actors have no worker assignment).
3. `WorkerAssignment.substrate_version` (field 6), stamped from the worker's
   pool label at assignment. The dispatcher routes by it with no fallback.

## Partition census (the price of "independent" stacks)

Fences that had to be ADDED for two stacks to coexist (upstream has none):

1. ate-api worker-pod informer: label-fenced to its own `substrate-version`
   (unfenced, either stack's syncer would crash the other's actors when their
   pods die — the sweep also releases actors).
2. ate-api stored-worker sweep (`enqueueStoredWorkers`): record-label fence,
   same reason.
3. atecontroller: cache-level label filter on WorkerPools (both stacks'
   controllers otherwise reconcile ALL pools under the same SSA field owner
   and silently overwrite each other forever).
4. atecontroller rendering: stamps `substrate-version` onto rendered
   Deployments and worker pods (worker pods otherwise carry no version).
5. atelet: no code fence, but hard NODE partitioning is mandatory (hostPort
   8085/9090 + hostPath + the dialer's exactly-one-atelet-per-node check).
   Version-labeled node pools are a structural requirement of the model.
6. Scheduler: fenced by data, not code — WorkerPool CR labels carry
   `substrate-version` and template selectors name it. The scheduler itself
   is version-blind; nothing prevents cross-version placement if labels lie.

Shared singletons that stayed shared (deliberately, with why):

- valkey + its Services/PVCs/init Job (the shared store IS the design).
- CRD definitions (incl. the relaxed ActorTemplate CRD, see hacks).
- The three doors: `api` (headless → dispatcher, permanent), `atenet-router`
  (selector carries the active version; the flip patches it), `dns`
  (unversioned selector — both stacks' dns pods answer identically).
- ALL ServiceAccounts. Three separate mechanisms pin SA names:
  GCS Workload Identity binds `ns/ate-system/sa/atelet`; the podidentity
  signer grants serverAuth EKU only to SA `atelet`; every worker's atunnel
  accepts exactly `spiffe://.../sa/atenet-router`. Suffixed SAs break TLS or
  storage access. SA identity is invisible-shared-state between stacks.
- podcertcontroller + both CA pools + ClusterTrustBundles: cluster
  infrastructure, single instance (two instances with different CA pools
  would flap the fixed-name trust bundles cluster-wide every ~5s).
- CA/JWT pool secrets, valkey-ca-certs, `ate-api-server-envvars` ConfigMap,
  OTLP collector config, ClusterRoles/Bindings.
- kube-dns stubDomains key: both stacks' dns controllers write byte-identical
  values (both read the shared door IPs), so dual writers converge without
  election — the "provably idempotent" branch of the house rule, measured.

Cross-version pairs actually exercised on the wire:

- green api → blue-written store records (every post-flip read).
- green worker restore ← blue-written GCS snapshot (+ blue-written local
  pause snapshot uploaded by blue atelet under #791, restored green).
- green router → blue atunnel (story-a/-c traffic pre-migration through the
  flipped door), authenticated because both stacks chain to the ONE
  podidentity CA and share the router SA identity.
- Control-plane actor-scoped RPCs crossing the door to the OLD side after the
  flip: exactly ONE in run 2 (story-a's suspend, routed by
  `worker_assignment.substrate_version=0.1.0` — the dispatcher's assignment
  lookup firing as designed; see RESULTS.md table). The deadline rung is a
  pod delete, not an RPC; commit-paused happens pre-flip (blue→blue). The
  compatibility surface collapsed to: store schema + snapshot format +
  tunnel TLS + the dispatcher.

## Hacks / carve-outs (the honest list)

1. ActorTemplate spec immutability (CRD CEL `self == oldSelf`) DROPPED
   wholesale. The doc assumed worker_selector was mutable; on main it is not.
   The real fix is #768's UpdateActorTemplate field mask; the POC relaxation
   makes the whole spec mutable.
2. Dispatcher rules propagate via ConfigMap volume: kubelet syncs ~1min
   behind the patch. Run 1 lost story-a's resume into that window (routed
   passthrough→old; the old api's fenced informer cannot see the claimed
   green pod; resume failed retryably and recovered). Fix: the operator now
   bounces the dispatcher pod after patching rules and waits for Ready. A
   real dispatcher would watch the ConfigMap via the API.
3. Operator runs out-of-cluster (laptop + kubeconfig), like the old POC. The
   Substrate CR + bundles are real; the operator's placement is scaffolding.
4. SIGTERM transport is still the kubectl-exec `runsc kill` borrow (#517
   ateom signal forwarding remains unlanded).
5. Pre-flip actor-selector scan skipped (assume no actor selector names the
   reserved key); CreateActor does not yet reject `substrate-version` in
   selectors. Both are cheap to add.
6. Dispatcher collapses caller identity: backends see the dispatcher's client
   cert, not the original caller's (bearer tokens do transit). Fine while the
   api does not authorize per-identity.
7. PodMonitoring dropped from the render; PDB kept shared across both stacks.
8. Run 2 (genuinely older blue binary + basis patches) not run tonight; the
   run-1 machinery (distinct ldflags-stamped images) is version-real but
   code-identical.

## Interaction table (dispatcher decisions, run 1: 912 calls)

| method | side | mode | reason | n |
|---|---|---|---|---|
| ListActors | new | upgrade | rule | 433 |
| ListActors | old | passthrough | rule | 409 |
| ResumeActor | old | passthrough | rule | 18 |
| CreateAtespace | old | passthrough | rule | 11 |
| GetActor | new | upgrade | rule | 8 |
| GetActor | old | passthrough | rule | 7 |
| ResumeActor | new | upgrade | rule | 7 |
| CreateActor | old | passthrough | rule | 6 |
| SuspendActor | old | passthrough | rule | 5 |
| ListWorkers | new | upgrade | rule | 3 |
| PauseActor | old | passthrough | rule | 1 |
| ListWorkers | old | passthrough | rule | 1 |
| CreateActor | new | upgrade | rule | 1 |

(ListActors bulk = operator gates/migrate polling. Zero `assignment-*` rows:
no post-flip actor-scoped call needed the old side in run 1.)

## Verdict inputs for "as simple as proposed?"

- One namespace, ~10 per-stack objects, 3 doors, 1 dispatcher, 6 code fences,
  3 basis changes, 1 CRD relaxation. The machinery ran unattended end to end.
- The riskiest illusions the POC killed: "template selector is already
  mutable" (it is not), "each stack only sees its own workers" (only after
  fencing), "SAs are just names" (they are cross-stack trust anchors).
- The surface that stays forever version-skewed even in this model: the
  shared store's record schema, the snapshot/manifest format, and the
  router↔atunnel TLS contract. Those need compatibility rules NO MATTER WHAT
  — dual-live shrinks the surface, it does not eliminate it.
