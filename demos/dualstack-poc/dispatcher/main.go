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

// Command dispatcher is a standing gRPC proxy that sits behind the api door
// during a dual-stack upgrade and forwards every ControlAPI, ActorIdentity,
// and Debug call to the old or new ate-api stack, per the routing rules file.
//
// Routing decisions are written as JSON lines on stdout (the interaction-table
// source); operational logs go to stderr so stdout stays machine-readable.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net"
	"os"
	"strings"

	"github.com/agent-substrate/substrate/internal/credbundle"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var (
	listenAddr           = pflag.String("listen", ":443", "Address and port the gRPC proxy listens on.")
	serverCredBundlePath = pflag.String("cred-bundle", "/run/servicedns.podcert.ate.dev/credential-bundle.pem", "File with the server TLS credential bundle.")
	backendCAPath        = pflag.String("backend-ca", "/run/servicedns-ca/trust-bundle.pem", "PEM trust bundle used to verify backend api serving certificates.")
	clientCredBundlePath = pflag.String("client-cred-bundle", "/run/podidentity.podcert.ate.dev/credential-bundle.pem", "Credential bundle presented as the client certificate when dialing backends.")
	oldTarget            = pflag.String("old", "dns:///ate-api-0-1-0.ate-system.svc:443", "gRPC target of the old stack's api service.")
	newTarget            = pflag.String("new", "dns:///ate-api-0-2-0.ate-system.svc:443", "gRPC target of the new stack's api service.")
	oldVersion           = pflag.String("old-version", "0.1.0", "substrate-version of the old stack.")
	newVersion           = pflag.String("new-version", "0.2.0", "substrate-version of the new stack.")
	rulesPath            = pflag.String("rules", "/etc/dispatcher/rules.json", "Routing rules file, polled every 2s.")
)

func main() {
	pflag.Parse()
	// Stdout is reserved for routing-decision JSON lines.
	log.SetOutput(os.Stderr)

	backendCAs, err := loadCertPool(*backendCAPath)
	if err != nil {
		log.Fatalf("load backend CA bundle: %v", err)
	}

	oldB, err := newBackend("old", *oldTarget, backendCAs)
	if err != nil {
		log.Fatalf("build old backend: %v", err)
	}
	newB, err := newBackend("new", *newTarget, backendCAs)
	if err != nil {
		log.Fatalf("build new backend: %v", err)
	}

	d := &dispatcher{
		old:        oldB,
		new:        newB,
		oldVersion: *oldVersion,
		rules:      startRules(*rulesPath),
	}

	serverCreds := credentials.NewTLS(&tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: credbundle.Loader(*serverCredBundlePath),
		// No client certs: auth is the backends' job, bearer tokens pass
		// through in metadata.
		ClientAuth: tls.NoClientCert,
	})

	lis, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("listen on %s: %v", *listenAddr, err)
	}

	srv := grpc.NewServer(grpc.Creds(serverCreds))
	ateapipb.RegisterControlServer(srv, d)
	ateapipb.RegisterActorIdentityServer(srv, d)
	ateapipb.RegisterDebugServer(srv, d)

	log.Printf("dispatcher listening on %s (old %s=%s, new %s=%s, rules=%s)",
		*listenAddr, *oldVersion, *oldTarget, *newVersion, *newTarget, *rulesPath)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// backend is one stack's api service. name is the "side" value in decision
// logs: "old" or "new".
type backend struct {
	name     string
	control  ateapipb.ControlClient
	identity ateapipb.ActorIdentityClient
	debug    ateapipb.DebugClient
}

// newBackend builds a lazy client conn to one stack's api service;
// grpc.NewClient does not connect until the first RPC. The api server's
// MaxConnectionAge GOAWAY churn is reconnected transparently.
func newBackend(name, target string, backendCAs *x509.CertPool) (*backend, error) {
	creds := credentials.NewTLS(&tls.Config{
		MinVersion:           tls.VersionTLS13,
		RootCAs:              backendCAs,
		ServerName:           targetHost(target),
		GetClientCertificate: credbundle.ClientLoader(*clientCredBundlePath),
	})
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("create %s backend client for %q: %w", name, target, err)
	}
	return &backend{
		name:     name,
		control:  ateapipb.NewControlClient(conn),
		identity: ateapipb.NewActorIdentityClient(conn),
		debug:    ateapipb.NewDebugClient(conn),
	}, nil
}

func loadCertPool(path string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("no certificates parsed from %s", path)
	}
	return pool, nil
}

// targetHost derives the TLS ServerName from a dns:/// gRPC target.
func targetHost(target string) string {
	t := strings.TrimPrefix(target, "dns:///")
	if host, _, err := net.SplitHostPort(t); err == nil {
		return host
	}
	return t
}
