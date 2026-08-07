// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"os"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/metadata"
)

// dispatcher proxies every Control, ActorIdentity, and Debug method (all
// unary) to one of the two stacks. Backend status errors are returned
// verbatim.
type dispatcher struct {
	ateapipb.UnimplementedControlServer
	ateapipb.UnimplementedActorIdentityServer
	ateapipb.UnimplementedDebugServer

	old, new   *backend
	oldVersion string
	rules      *rulesWatcher
}

// lookupMethods must run against the stack the actor is currently assigned
// to; in upgrade mode they are routed by a GetActor lookup. Everything else
// (creates, resumes, reads, lists, atespace ops, tags, snapshots, identity,
// debug) goes to the new stack.
var lookupMethods = map[string]bool{
	"SuspendActor": true,
	"PauseActor":   true,
	"DeleteActor":  true,
	"UpdateActor":  true,
}

type decision struct {
	b      *backend
	mode   string
	reason string
}

// route picks a backend. The lookup GetActor runs on the new stack — both
// stacks share the store, so it sees every actor.
func (d *dispatcher) route(ctx context.Context, method string, actor *ateapipb.ObjectRef) decision {
	mode := d.rules.Mode()
	if mode == modePassthrough {
		return decision{d.old, mode, "rule"}
	}
	if !lookupMethods[method] || actor == nil {
		return decision{d.new, mode, "rule"}
	}
	logDecision("GetActor", refString(actor), d.new.name, mode, "dispatcher-lookup")
	a, err := d.new.control.GetActor(passthroughMD(ctx), &ateapipb.GetActorRequest{Actor: actor})
	if err != nil {
		// Forward to the new stack, which surfaces the same error.
		return decision{d.new, mode, "lookup-error"}
	}
	assignment := a.GetWorkerAssignment()
	if assignment == nil {
		return decision{d.new, mode, "no-assignment"}
	}
	v := assignment.GetSubstrateVersion()
	if v == d.oldVersion {
		return decision{d.old, mode, "assignment-" + v}
	}
	if v == "" {
		v = "unversioned"
	}
	return decision{d.new, mode, "assignment-" + v}
}

// forward logs the routing decision and invokes call against the chosen
// backend with all incoming metadata copied through (bearer auth transits).
func forward[T any](ctx context.Context, method, actor string, dec decision, call func(context.Context, *backend) (T, error)) (T, error) {
	logDecision(method, actor, dec.b.name, dec.mode, dec.reason)
	return call(passthroughMD(ctx), dec.b)
}

func passthroughMD(ctx context.Context) context.Context {
	md, _ := metadata.FromIncomingContext(ctx)
	return metadata.NewOutgoingContext(ctx, md.Copy())
}

func refString(ref *ateapipb.ObjectRef) string {
	if ref == nil {
		return ""
	}
	return ref.GetAtespace() + "/" + ref.GetName()
}

func metadataRef(md *ateapipb.ResourceMetadata) *ateapipb.ObjectRef {
	if md == nil {
		return nil
	}
	return &ateapipb.ObjectRef{Atespace: md.GetAtespace(), Name: md.GetName()}
}

func snapshotRefString(ref *ateapipb.ActorSnapshotRef) string {
	if s := ref.GetSnapshot(); s != nil {
		return refString(s)
	}
	return refString(ref.GetTag())
}

type decisionLine struct {
	M      string `json:"m"`
	Actor  string `json:"actor"`
	Side   string `json:"side"`
	Mode   string `json:"mode"`
	Reason string `json:"reason"`
}

// logDecision writes one JSON line per routing decision to stdout.
func logDecision(method, actor, side, mode, reason string) {
	raw, err := json.Marshal(decisionLine{M: method, Actor: actor, Side: side, Mode: mode, Reason: reason})
	if err != nil {
		return
	}
	_, _ = os.Stdout.Write(append(raw, '\n'))
}

// Control.

func (d *dispatcher) GetActor(ctx context.Context, req *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
	return forward(ctx, "GetActor", refString(req.GetActor()), d.route(ctx, "GetActor", req.GetActor()),
		func(ctx context.Context, b *backend) (*ateapipb.Actor, error) {
			return b.control.GetActor(ctx, req)
		})
}

func (d *dispatcher) CreateActor(ctx context.Context, req *ateapipb.CreateActorRequest) (*ateapipb.Actor, error) {
	ref := metadataRef(req.GetActor().GetMetadata())
	return forward(ctx, "CreateActor", refString(ref), d.route(ctx, "CreateActor", ref),
		func(ctx context.Context, b *backend) (*ateapipb.Actor, error) {
			return b.control.CreateActor(ctx, req)
		})
}

func (d *dispatcher) UpdateActor(ctx context.Context, req *ateapipb.UpdateActorRequest) (*ateapipb.Actor, error) {
	ref := metadataRef(req.GetActor().GetMetadata())
	return forward(ctx, "UpdateActor", refString(ref), d.route(ctx, "UpdateActor", ref),
		func(ctx context.Context, b *backend) (*ateapipb.Actor, error) {
			return b.control.UpdateActor(ctx, req)
		})
}

func (d *dispatcher) SuspendActor(ctx context.Context, req *ateapipb.SuspendActorRequest) (*ateapipb.SuspendActorResponse, error) {
	return forward(ctx, "SuspendActor", refString(req.GetActor()), d.route(ctx, "SuspendActor", req.GetActor()),
		func(ctx context.Context, b *backend) (*ateapipb.SuspendActorResponse, error) {
			return b.control.SuspendActor(ctx, req)
		})
}

func (d *dispatcher) PauseActor(ctx context.Context, req *ateapipb.PauseActorRequest) (*ateapipb.PauseActorResponse, error) {
	return forward(ctx, "PauseActor", refString(req.GetActor()), d.route(ctx, "PauseActor", req.GetActor()),
		func(ctx context.Context, b *backend) (*ateapipb.PauseActorResponse, error) {
			return b.control.PauseActor(ctx, req)
		})
}

func (d *dispatcher) ResumeActor(ctx context.Context, req *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
	return forward(ctx, "ResumeActor", refString(req.GetActor()), d.route(ctx, "ResumeActor", req.GetActor()),
		func(ctx context.Context, b *backend) (*ateapipb.ResumeActorResponse, error) {
			return b.control.ResumeActor(ctx, req)
		})
}

func (d *dispatcher) DeleteActor(ctx context.Context, req *ateapipb.DeleteActorRequest) (*ateapipb.Actor, error) {
	return forward(ctx, "DeleteActor", refString(req.GetActor()), d.route(ctx, "DeleteActor", req.GetActor()),
		func(ctx context.Context, b *backend) (*ateapipb.Actor, error) {
			return b.control.DeleteActor(ctx, req)
		})
}

func (d *dispatcher) GetActorSnapshot(ctx context.Context, req *ateapipb.GetActorSnapshotRequest) (*ateapipb.ActorSnapshot, error) {
	return forward(ctx, "GetActorSnapshot", snapshotRefString(req.GetSnapshot()), d.route(ctx, "GetActorSnapshot", nil),
		func(ctx context.Context, b *backend) (*ateapipb.ActorSnapshot, error) {
			return b.control.GetActorSnapshot(ctx, req)
		})
}

func (d *dispatcher) ListActorSnapshots(ctx context.Context, req *ateapipb.ListActorSnapshotsRequest) (*ateapipb.ListActorSnapshotsResponse, error) {
	return forward(ctx, "ListActorSnapshots", req.GetAtespace(), d.route(ctx, "ListActorSnapshots", nil),
		func(ctx context.Context, b *backend) (*ateapipb.ListActorSnapshotsResponse, error) {
			return b.control.ListActorSnapshots(ctx, req)
		})
}

func (d *dispatcher) TagActorSnapshot(ctx context.Context, req *ateapipb.TagActorSnapshotRequest) (*ateapipb.ActorSnapshotTag, error) {
	return forward(ctx, "TagActorSnapshot", refString(metadataRef(req.GetTag().GetMetadata())), d.route(ctx, "TagActorSnapshot", nil),
		func(ctx context.Context, b *backend) (*ateapipb.ActorSnapshotTag, error) {
			return b.control.TagActorSnapshot(ctx, req)
		})
}

func (d *dispatcher) UpdateActorSnapshotTag(ctx context.Context, req *ateapipb.UpdateActorSnapshotTagRequest) (*ateapipb.ActorSnapshotTag, error) {
	return forward(ctx, "UpdateActorSnapshotTag", refString(metadataRef(req.GetTag().GetMetadata())), d.route(ctx, "UpdateActorSnapshotTag", nil),
		func(ctx context.Context, b *backend) (*ateapipb.ActorSnapshotTag, error) {
			return b.control.UpdateActorSnapshotTag(ctx, req)
		})
}

func (d *dispatcher) DeleteActorSnapshotTag(ctx context.Context, req *ateapipb.DeleteActorSnapshotTagRequest) (*ateapipb.ActorSnapshotTag, error) {
	return forward(ctx, "DeleteActorSnapshotTag", refString(req.GetTag()), d.route(ctx, "DeleteActorSnapshotTag", nil),
		func(ctx context.Context, b *backend) (*ateapipb.ActorSnapshotTag, error) {
			return b.control.DeleteActorSnapshotTag(ctx, req)
		})
}

func (d *dispatcher) ListWorkers(ctx context.Context, req *ateapipb.ListWorkersRequest) (*ateapipb.ListWorkersResponse, error) {
	return forward(ctx, "ListWorkers", "", d.route(ctx, "ListWorkers", nil),
		func(ctx context.Context, b *backend) (*ateapipb.ListWorkersResponse, error) {
			return b.control.ListWorkers(ctx, req)
		})
}

func (d *dispatcher) ListActors(ctx context.Context, req *ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
	return forward(ctx, "ListActors", req.GetAtespace(), d.route(ctx, "ListActors", nil),
		func(ctx context.Context, b *backend) (*ateapipb.ListActorsResponse, error) {
			return b.control.ListActors(ctx, req)
		})
}

func (d *dispatcher) CreateAtespace(ctx context.Context, req *ateapipb.CreateAtespaceRequest) (*ateapipb.Atespace, error) {
	return forward(ctx, "CreateAtespace", req.GetAtespace().GetMetadata().GetName(), d.route(ctx, "CreateAtespace", nil),
		func(ctx context.Context, b *backend) (*ateapipb.Atespace, error) {
			return b.control.CreateAtespace(ctx, req)
		})
}

func (d *dispatcher) GetAtespace(ctx context.Context, req *ateapipb.GetAtespaceRequest) (*ateapipb.Atespace, error) {
	return forward(ctx, "GetAtespace", req.GetAtespace().GetName(), d.route(ctx, "GetAtespace", nil),
		func(ctx context.Context, b *backend) (*ateapipb.Atespace, error) {
			return b.control.GetAtespace(ctx, req)
		})
}

func (d *dispatcher) ListAtespaces(ctx context.Context, req *ateapipb.ListAtespacesRequest) (*ateapipb.ListAtespacesResponse, error) {
	return forward(ctx, "ListAtespaces", "", d.route(ctx, "ListAtespaces", nil),
		func(ctx context.Context, b *backend) (*ateapipb.ListAtespacesResponse, error) {
			return b.control.ListAtespaces(ctx, req)
		})
}

func (d *dispatcher) DeleteAtespace(ctx context.Context, req *ateapipb.DeleteAtespaceRequest) (*ateapipb.Atespace, error) {
	return forward(ctx, "DeleteAtespace", req.GetAtespace().GetName(), d.route(ctx, "DeleteAtespace", nil),
		func(ctx context.Context, b *backend) (*ateapipb.Atespace, error) {
			return b.control.DeleteAtespace(ctx, req)
		})
}

// ActorIdentity.

func (d *dispatcher) MintJWT(ctx context.Context, req *ateapipb.MintJWTRequest) (*ateapipb.MintJWTResponse, error) {
	return forward(ctx, "MintJWT", req.GetAtespace()+"/"+req.GetActorName(), d.route(ctx, "MintJWT", nil),
		func(ctx context.Context, b *backend) (*ateapipb.MintJWTResponse, error) {
			return b.identity.MintJWT(ctx, req)
		})
}

func (d *dispatcher) MintCert(ctx context.Context, req *ateapipb.MintCertRequest) (*ateapipb.MintCertResponse, error) {
	return forward(ctx, "MintCert", req.GetAtespace()+"/"+req.GetActorName(), d.route(ctx, "MintCert", nil),
		func(ctx context.Context, b *backend) (*ateapipb.MintCertResponse, error) {
			return b.identity.MintCert(ctx, req)
		})
}

// Debug.

func (d *dispatcher) DebugClear(ctx context.Context, req *ateapipb.DebugClearRequest) (*ateapipb.DebugClearResponse, error) {
	return forward(ctx, "DebugClear", "", d.route(ctx, "DebugClear", nil),
		func(ctx context.Context, b *backend) (*ateapipb.DebugClearResponse, error) {
			return b.debug.DebugClear(ctx, req)
		})
}
