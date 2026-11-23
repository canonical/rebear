// Copyright 2026 Canonical Inc.
// Originally by Gustavo Niemeyer.
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

package harness_test

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/canonical/rebear/internal/harness"
)

// startUpstream starts the harness server playing the endpoint an
// outlet connects to, and returns it with its base URL.
func startUpstream(t *testing.T, reply harness.Response) (*harness.Server, string) {
	t.Helper()
	s := harness.NewServer(reply)
	url, err := s.Start()
	if err != nil {
		t.Fatalf("cannot start server: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	return s, url
}

func TestServerRecordsRequest(t *testing.T) {
	// A request lands in the record with its method, path, headers,
	// and body; the canned reply comes back with status and headers.
	s, url := startUpstream(t, harness.Response{
		Status:  201,
		Headers: map[string]string{"X-Probe": "yes"},
		Body:    "pong",
	})
	resp, err := harness.NewClient(url).Do("POST", "/v1/messages", map[string]string{"X-Ask": "ping"}, "ping-body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 201 {
		t.Errorf("Status = %d, want 201", resp.Status)
	}
	if resp.Headers["X-Probe"] != "yes" {
		t.Errorf("X-Probe = %q, want yes", resp.Headers["X-Probe"])
	}
	if resp.Body != "pong" {
		t.Errorf("Body = %q, want pong", resp.Body)
	}

	reqs := s.Requests()
	if len(reqs) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(reqs))
	}
	req := reqs[0]
	if req.Method != "POST" || req.Path != "/v1/messages" {
		t.Errorf("recorded %s %s, want POST /v1/messages", req.Method, req.Path)
	}
	if req.Headers["X-Ask"] != "ping" {
		t.Errorf("X-Ask = %q, want ping", req.Headers["X-Ask"])
	}
	if req.Body != "ping-body" {
		t.Errorf("Body = %q, want ping-body", req.Body)
	}
}

func TestServerChunks(t *testing.T) {
	// A chunked reply delivers the chunks in order, concatenated in
	// the received body.
	_, url := startUpstream(t, harness.Response{
		Status: 200,
		Chunks: []string{"{\"a\":", "1}"},
	})
	client := harness.NewClient(url)
	resp, err := client.Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != "{\"a\":1}" {
		t.Errorf("Body = %q, want %q", resp.Body, "{\"a\":1}")
	}
}

func TestServerSetReply(t *testing.T) {
	// SetReply changes what the next request gets, without affecting
	// the record.
	s, url := startUpstream(t, harness.Response{Status: 200, Body: "first"})
	client := harness.NewClient(url)
	if _, err := client.Do("GET", "/", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s.SetReply(harness.Response{Status: 200, Body: "second"})
	resp, err := client.Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != "second" {
		t.Errorf("Body = %q, want second", resp.Body)
	}
	if len(s.Requests()) != 2 {
		t.Errorf("recorded %d requests, want 2", len(s.Requests()))
	}
}

func TestServerSnapshotIndependence(t *testing.T) {
	// The snapshot is a copy: mutating it cannot rewrite history.
	s, url := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	client := harness.NewClient(url)
	if _, err := client.Do("GET", "/", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	snapshot := s.Requests()
	snapshot[0].Body = "tampered"
	if s.Requests()[0].Body != "" {
		t.Errorf("later snapshot = %q, want untampered", s.Requests()[0].Body)
	}
}

func TestClientUnixSocket(t *testing.T) {
	// The same client dials a unix socket when the base URL says
	// unix:, proving the path without a rebear inlet in place.
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "sock"))
	if err != nil {
		t.Fatalf("cannot listen: %v", err)
	}
	s := harness.NewServer(harness.Response{Status: 200, Body: "unix-pong"})
	go func() { _ = s.ServeOn(ln) }()
	t.Cleanup(func() { _ = ln.Close(); _ = s.Stop() })

	resp, err := harness.NewClient("unix:"+ln.Addr().String()).Do("GET", "/probe", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != "unix-pong" {
		t.Errorf("Body = %q, want unix-pong", resp.Body)
	}
	if s.Requests()[0].Path != "/probe" {
		t.Errorf("Path = %q, want /probe", s.Requests()[0].Path)
	}
}

func TestClientStoppedServer(t *testing.T) {
	// A request to a stopped server fails with an error rather than
	// hanging: the timeout here only guards the test itself.
	s, url := startUpstream(t, harness.Response{Status: 200, Body: ""})
	if err := s.Stop(); err != nil {
		t.Fatalf("cannot stop: %v", err)
	}
	client := harness.NewClient(url)
	done := make(chan error, 1)
	go func() {
		_, err := client.Do("GET", "/", nil, "")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("error = nil, want a failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("request to a stopped server did not return")
	}
}
