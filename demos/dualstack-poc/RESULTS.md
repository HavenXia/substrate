# POC run results (GKE substrate-poc, 2026-08-07)

Three full end-to-end runs of: fresh install at 0.1.0 → seed 5 actor stories →
`kubectl patch substrate` to 0.2.0 → unattended walk to Ready-at-0.2.0.
Runs 1-2: both stacks same code, different version stamps (the machinery
test). Run 3 = the design doc's "Run 2": blue genuinely older. Raw artifacts
in `results/`.

## Run 3 (old blue, real version boundary): ALL PASS, zero interventions

Blue built from upstream `2fe26c16` + the basis cherry-picks (branch
`poc-blue-old`; two small #705 compat shims were the only backport cost).
That base is seven merges behind green — the window includes the gVisor
release bump (#787), pause-snapshot pruning (#705), snapshot-tag CAS changes
(#755/#758) and Assignment schema churn. All five stories passed identically
to run 2, including: suspend snapshot written by the OLD api restored by the
NEW stack with RAM intact (3→5→6), the pause snapshot uploaded by the OLD
atelet under #791 restored green (3→4), and green-written records read back
through the old binary-proto store code without complaint. Dispatcher table
identical in shape to run 2 — again exactly ONE old-side call post-flip
(SuspendActor, reason assignment-0.1.0). Logs: results/{operator,dispatcher}-run3*.

## Run 2 (clean, after the run-1 fix): ALL PASS, zero interventions

Operator phase log (results/operator-run2.log):

```
01:34:07 Pending    -> Installing  installing 0.1.0
01:35:05 Installing -> Ready       installed 0.1.0; door serving          (58s)
01:35:59 Ready      -> GreenUp     upgrading 0.1.0 -> 0.2.0
01:36:21 GreenUp    -> CommitPaused green up; preflight: 6 actors, 8 workers readable
01:36:26 CommitPaused -> Flip      no paused actors remain                (story-e committed via #791)
01:36:35 Flip       -> Migrate     door+rules flipped, 2 templates repointed; grace 90s
01:38:09 Migrate    -> Teardown    no actors left on the old stack
01:38:13 Teardown   -> Ready       upgrade to 0.2.0 complete: 1 killed, 1 committed
```

Bump→Ready: 2m14s wall (grace 90s dominates). Harness: 4 seed PASS,
5 verify-during PASS (story-a: SIGTERM → drain-ready → suspend routed blue by
assignment → resume landed green, counter 4→5→6 monotonic; one 15.3s traffic
gap during the flip window), 8 verify-after PASS (b born green; c snapshot
restored cross-version, 3→4→5; e pause-committed then green, 3→4; d CRASHED,
zero 0.1.0 workers left).

Dispatcher interaction table, upgrade mode (results/dispatcher-run2.jsonl —
log starts at the flip bounce):

| method | side | reason | n |
|---|---|---|---|
| ListActors | new | rule | 228 |
| GetActor | new | rule | 12 |
| ResumeActor | new | rule | 11 |
| ListWorkers | new | rule | 2 |
| GetActor | new | dispatcher-lookup | 1 |
| **SuspendActor** | **old** | **assignment-0.1.0** | **1** |
| CreateActor | new | rule | 1 |

The single old-side row is story-a's post-flip suspend: the dispatcher looked
the actor up, saw `worker_assignment.substrate_version=0.1.0`, and routed it
to the blue api. That is the entire post-flip cross-stack control-plane
traffic: one deliberate call.

## Run 1: PASS with one raced resume (root-caused, fixed)

Same walk, 7m30s bump→Ready. story-a's suspend+resume landed inside the
~1min kubelet ConfigMap-volume sync window after the operator patched the
dispatcher rules: still passthrough → old api → its version-fenced informer
cannot see the green pod the scheduler had claimed → resume failed
(retryable); one manual ResumeActor recovered it via the RESUMING recovery
path. Fix (run 2): the operator bounces the dispatcher after patching rules
and waits for Ready; the harness also treats that error as retryable.
Run-1 decisions: results/dispatcher-run1.jsonl (912 calls, incl. the
passthrough race evidence).

## Rerun recipe (~7 min, warm cluster)

```
cd /tmp/sub-poc-dual
./demos/dualstack-poc/wipe.sh && ./demos/dualstack-poc/bootstrap.sh
kubectl apply -f demos/dualstack-poc/render/out/crds.yaml
(cd demos/dualstack-poc/operator && go run . --bundle-dir ../render/out --kubecontext gke_haoyuxia-gke-dev_us-central1-c_substrate-poc &)
kubectl apply -f /tmp/poc-run/substrate-cr.yaml            # version 0.1.0, graceSeconds 90
# wait: kubectl get substrate -w  → Ready; then:
(cd demos/dualstack-poc/harness && go run . --kubecontext ... --atespace poc --phase seed)
kubectl patch substrate substrate --type merge -p '{"spec":{"version":"0.2.0"}}'
(cd demos/dualstack-poc/harness && go run . --kubecontext ... --atespace poc --phase verify-during)
# after Ready at 0.2.0:
(cd demos/dualstack-poc/harness && go run . --kubecontext ... --atespace poc --phase verify-after)
```

The cluster is currently LEFT IN the post-run-2 state: `Ready 0.2.0`,
stories a/b/c/e RUNNING green, story-d CRASHED (by design) — inspectable live.
