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

package proxy_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/canonical/rebear/internal/harness"
	"github.com/canonical/rebear/internal/logging"
	"github.com/canonical/rebear/internal/notes"
	"github.com/canonical/rebear/internal/proxy"
	"github.com/canonical/rebear/internal/setup"
	"github.com/canonical/rebear/internal/snippets"
	rebeartls "github.com/canonical/rebear/internal/tls"
)

// reindent strips each line up to and including its first "|",
// keeping the suffix as the line, and drops lines without one.
// It lets cnf samples sit pipe-prefixed and indented at the
// depth of the code around them.
func reindent(sample string) string {
	var out strings.Builder
	for _, line := range strings.Split(strings.Trim(sample, "\n"), "\n") {
		_, content, found := strings.Cut(line, "|")
		if !found {
			continue
		}
		out.WriteString(content)
		out.WriteString("\n")
	}
	return out.String()
}

// addrOf strips the scheme off a base URL, leaving the net address.
func addrOf(base string) string {
	return strings.TrimPrefix(base, "http://")
}

// startUpstream starts a harness server to play the outlet's endpoint, and
// returns it with its base URL.
func startUpstream(t *testing.T, reply harness.Response) (*harness.Server, string) {
	t.Helper()
	s := harness.NewServer(reply)
	url, err := s.Start()
	if err != nil {
		t.Fatalf("cannot start upstream: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	return s, url
}

// startProxy loads setup text as a config and starts a proxy on it. The
// cnf dir anchors relative unix socket paths.
func startProxy(t *testing.T, cnf string) *proxy.Proxy {
	t.Helper()
	dir := t.TempDir()
	cfg, err := setup.Parse([]byte(cnf), dir)
	if err != nil {
		t.Fatalf("cannot parse setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p, err := proxy.New(&proxy.ProxyOptions{Setup: cfg})
	if err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	p.Serve(ctx)
	t.Cleanup(func() { _ = p.Stop() })
	return p
}

// freePort grabs a loopback port the inlet can listen on. The
// listen-then-close race is acceptable for tests.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot grab a port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestProxyTCPRoundTrip(t *testing.T) {
	// A tcp inlet request reaches the outlet with method, headers,
	// and body; the response streams back unchanged.
	upstream, upURL := startUpstream(t, harness.Response{Status: 201, Headers: map[string]string{"X-Up": "yes"}, Body: "hello"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "upstream"
		|outlets.upstream:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)

	resp, err := harness.NewClient("http://"+addr).Do("POST", "/v1/messages", map[string]string{"X-Ask": "ping"}, "ping-body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 201 || resp.Body != "hello" || resp.Headers["X-Up"] != "yes" {
		t.Errorf("response = %d %q %v", resp.Status, resp.Body, resp.Headers)
	}

	req := upstream.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if req.Method != "POST" {
		t.Errorf("Method = %q, want POST", req.Method)
	}
	if req.Headers["X-Ask"] != "ping" {
		t.Errorf("X-Ask = %q, want ping", req.Headers["X-Ask"])
	}
	if req.Body != "ping-body" {
		t.Errorf("Body = %q, want ping-body", req.Body)
	}
}

func TestProxyInletOutletFromURL(t *testing.T) {
	// Endpoints may name only a url with a host: the address is
	// its host, so the inlet listens and the outlet dials there.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    url: "http://` + addr + `/v1"
		|    outlet: "up"
		|outlets.up:
		|    url: "` + upURL + `/api"
		`)
	_ = startProxy(t, cnf)

	if _, err := harness.NewClient("http://"+addr).Do("GET", "/v1/x", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := upstream.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if req.Path != "/api/x" {
		t.Errorf("Path = %q, want /api/x", req.Path)
	}
}

func TestProxyIPVhostIgnored(t *testing.T) {
	// A url naming an IP constrains nothing: the vhost match
	// skips it, so a request sending any Host still serves.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    url: "http://` + addr + `/v1"
		|    outlet: "up"
		|outlets.up:
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)

	// The vhost would be 127.0.0.1:port; a request naming another
	// host still serves rather than 404ing.
	resp, err := harness.NewVhostClient("http://"+addr, "anything.example.com").Do("GET", "/v1/x", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("Status = %d, want 200: an IP vhost matches any host", resp.Status)
	}
	if req := upstream.LastRequest(); req == nil || req.Path != "/x" {
		t.Errorf("outlet path = %v, want /x", req)
	}
}

func TestProxyLocalhostVhostIgnored(t *testing.T) {
	// The name localhost in an inlet url, as an IP, names the
	// address alone: a request sending any Host still serves.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	_, port, _ := net.SplitHostPort(addr)
	cnf := reindent(`
		|inlets.local:
		|    url: "http://localhost:` + port + `/v1"
		|    outlet: "up"
		|outlets.up:
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)

	resp, err := harness.NewVhostClient("http://"+addr, "anything.example.com").Do("GET", "/v1/x", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("Status = %d, want 200: a localhost vhost matches any host", resp.Status)
	}
	if req := upstream.LastRequest(); req == nil || req.Path != "/x" {
		t.Errorf("outlet path = %v, want /x", req)
	}
}

func TestProxyEscapedPathPassthrough(t *testing.T) {
	// A %2F in the trailing reaches the outlet escaped: the raw
	// path is preserved, cut by the same mount.
	upstream, remoteURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    url: "http://` + addr + `/v1"
		|    outlet: "remote"
		|outlets.remote:
		|    url: "` + remoteURL + `/api"
		`)
	_ = startProxy(t, cnf)

	if _, err := harness.NewClient("http://"+addr).Do("GET", "/v1/a%2Fb", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := upstream.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if req.Path != "/api/a/b" {
		t.Errorf("Path = %q, want the decoded /api/a/b", req.Path)
	}
	if req.RawPath != "/api/a%2Fb" {
		t.Errorf("RawPath = %q, want the escaped %q", req.RawPath, "/api/a%2Fb")
	}
}

func TestProxyEscapedMountCut(t *testing.T) {
	// Escapes inside the mount region - in the mount's own url
	// and in the request alike - count as one byte each in the
	// raw cut, mirroring the decoded match: they are consumed,
	// not leaked into the trailing.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    url: "http://` + addr + `/a%2Fb/c"
		|    outlet: "remote"
		|outlets.remote:
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)

	if _, err := harness.NewClient("http://"+addr).Do("GET", "/a/b%2Fc/x", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := upstream.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if req.Path != "/x" || req.RawPath != "/x" {
		t.Errorf("outlet path = %q raw %q, want /x and /x", req.Path, req.RawPath)
	}
}

func TestTrimEscaped(t *testing.T) {
	// The raw cut walks beside the mount, verifying every byte:
	// a literal must equal its mount byte, a %XX triple must
	// decode to it - matching length alone is not enough.
	for _, test := range []struct {
		raw, mount, want string
		ok               bool
	}{
		{raw: "/a%2Fb/c", mount: "/a/b/c", want: "", ok: true},
		{raw: "/x/a%2Fb", mount: "/x", want: "/a%2Fb", ok: true},
		{raw: "/a/b%2Fc/x", mount: "/a/b/c", want: "/x", ok: true},
		{raw: "/a%2Fb", mount: "", want: "/a%2Fb", ok: true},
		{raw: "/%61%2F%62", mount: "/a/b", want: "", ok: true},
		{raw: "/x", mount: "/x/y", want: "", ok: false},
		{raw: "/a%58b/c", mount: "/a/b/c", want: "", ok: false},
		{raw: "/x/a%2Fb", mount: "/y", want: "", ok: false},
		{raw: "/a%", mount: "/a/b", want: "", ok: false},
		{raw: "/a%ZZ", mount: "/a/b", want: "", ok: false},
	} {
		got, ok := proxy.TrimEscaped(test.raw, test.mount)
		if got != test.want || ok != test.ok {
			t.Errorf("trimEscaped(%q, %q) = %q, %v, want %q, %v", test.raw, test.mount, got, ok, test.want, test.ok)
		}
	}
}

func TestProxyMountCut(t *testing.T) {
	// The inlet url's path is cut off before routing; the trailing
	// path lands on the outlet url. The query is untouched.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	sock := filepath.Join(t.TempDir(), "mount.sock")
	cnf := reindent(`
		|inlets.local:
		|    address: "` + sock + `"
		|    url: "/v1"
		|    outlet: "upstream"
		|outlets.upstream:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `/api"
		`)
	dir := filepath.Dir(sock)
	cfg, err := setup.Parse([]byte(cnf), dir)
	if err != nil {
		t.Fatalf("cannot parse setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p, err := proxy.New(&proxy.ProxyOptions{Setup: cfg})
	if err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	p.Serve(ctx)

	client := harness.NewClient("unix:" + sock)
	if _, err := client.Do("GET", "/v1/messages?x=1", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := upstream.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if req.Path != "/api/messages" {
		t.Errorf("Path = %q, want /api/messages", req.Path)
	}
}

func TestProxyUnixOutlet(t *testing.T) {
	// A unix outlet dials the socket and builds the outgoing URL from
	// its url setting: the host names the service, the path prefixes
	// the trailing request path.
	up := harness.NewServer(harness.Response{Status: 200, Body: "ok"})
	upsock := filepath.Join(t.TempDir(), "up.sock")
	ln, err := net.Listen("unix", upsock)
	if err != nil {
		t.Fatalf("cannot listen on outlet socket: %v", err)
	}
	if _, err := up.StartOn(ln); err != nil {
		t.Fatalf("cannot start upstream: %v", err)
	}
	t.Cleanup(func() { _ = up.Stop() })

	insock := filepath.Join(t.TempDir(), "in.sock")
	cnf := reindent(`
		|inlets.local:
		|    address: "` + insock + `"
		|    outlet: "upstream"
		|outlets.upstream:
		|    address: "` + upsock + `"
		|    url: "http://internal.example/api"
		`)
	dir := filepath.Dir(insock)
	cfg, err := setup.Parse([]byte(cnf), dir)
	if err != nil {
		t.Fatalf("cannot parse setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p, err := proxy.New(&proxy.ProxyOptions{Setup: cfg})
	if err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	p.Serve(ctx)

	client := harness.NewClient("unix:" + insock)
	if _, err := client.Do("GET", "/messages", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := up.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if req.Path != "/api/messages" {
		t.Errorf("Path = %q, want /api/messages", req.Path)
	}
	if req.Host != "internal.example" {
		t.Errorf("Host = %q, want internal.example", req.Host)
	}
}

func TestProxyUnixOutletDerivedURL(t *testing.T) {
	// Without a url, a unix outlet speaks the endpoint's local
	// name: only the socket changes, and the client's host stops
	// riding the conversation.
	up := harness.NewServer(harness.Response{Status: 200, Body: "ok"})
	upsock := filepath.Join(t.TempDir(), "up.sock")
	ln, err := net.Listen("unix", upsock)
	if err != nil {
		t.Fatalf("cannot listen on outlet socket: %v", err)
	}
	if _, err := up.StartOn(ln); err != nil {
		t.Fatalf("cannot start upstream: %v", err)
	}
	t.Cleanup(func() { _ = up.Stop() })

	insock := filepath.Join(t.TempDir(), "in.sock")
	cnf := reindent(`
		|inlets.local:
		|    address: "` + insock + `"
		|    outlet: "upstream"
		|outlets.upstream:
		|    address: "` + upsock + `"
		`)
	dir := filepath.Dir(insock)
	cfg, err := setup.Parse([]byte(cnf), dir)
	if err != nil {
		t.Fatalf("cannot parse setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p, err := proxy.New(&proxy.ProxyOptions{Setup: cfg})
	if err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	p.Serve(ctx)

	// The unix client speaks http to a fake host over the socket:
	// the outlet names the endpoint instead.
	client := harness.NewClient("unix:" + insock)
	if _, err := client.Do("GET", "/messages", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := up.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if req.Path != "/messages" {
		t.Errorf("Path = %q, want /messages", req.Path)
	}
	if req.Host != "localhost" {
		t.Errorf("Host = %q, want localhost", req.Host)
	}
}

func TestProxyUnixOutletPathOnlyURL(t *testing.T) {
	// A path-only url on a unix outlet keeps speaking the
	// endpoint's local name - the socket names no host - while
	// the path still prefixes the outgoing one.
	up := harness.NewServer(harness.Response{Status: 200, Body: "ok"})
	upsock := filepath.Join(t.TempDir(), "up.sock")
	ln, err := net.Listen("unix", upsock)
	if err != nil {
		t.Fatalf("cannot listen on outlet socket: %v", err)
	}
	if _, err := up.StartOn(ln); err != nil {
		t.Fatalf("cannot start upstream: %v", err)
	}
	t.Cleanup(func() { _ = up.Stop() })

	insock := filepath.Join(t.TempDir(), "in.sock")
	cnf := reindent(`
		|inlets.local:
		|    address: "` + insock + `"
		|    outlet: "upstream"
		|outlets.upstream:
		|    address: "` + upsock + `"
		|    url: "/api"
		`)
	dir := filepath.Dir(insock)
	cfg, err := setup.Parse([]byte(cnf), dir)
	if err != nil {
		t.Fatalf("cannot parse setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p, err := proxy.New(&proxy.ProxyOptions{Setup: cfg})
	if err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	p.Serve(ctx)

	client := harness.NewClient("unix:" + insock)
	if _, err := client.Do("GET", "/messages", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := up.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if req.Path != "/api/messages" {
		t.Errorf("Path = %q, want /api/messages", req.Path)
	}
	if req.Host != "localhost" {
		t.Errorf("Host = %q, want localhost", req.Host)
	}
}

func TestProxyUnixOutletTLS(t *testing.T) {
	// A unix outlet with TLS settings speaks https over the
	// socket wherever the url leaves the scheme open - no url
	// or a path-only one - and the endpoint's local name carries
	// the conversation and the verification.
	testCases := []struct {
		name string
		url  string
		path string
	}{
		{"no url", "", "/messages"},
		{"path only", "/api", "/api/messages"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tlsDir := t.TempDir()
			store := rebeartls.NewStore(tlsDir)
			// The anchor speaks the outlet's local name.
			anchor, err := store.Pair("ca1", "localhost", true)
			if err != nil {
				t.Fatalf("cannot create anchor: %v", err)
			}
			upstream := harness.NewServer(harness.Response{Status: 200, Body: "hi"})
			upsock := filepath.Join(t.TempDir(), "up.sock")
			ln, err := net.Listen("unix", upsock)
			if err != nil {
				t.Fatalf("cannot listen on outlet socket: %v", err)
			}
			upstreamCfg := &tls.Config{Certificates: []tls.Certificate{*anchor.Full}}
			if _, err := upstream.StartOn(tls.NewListener(ln, upstreamCfg)); err != nil {
				t.Fatalf("cannot start upstream: %v", err)
			}
			t.Cleanup(func() { _ = upstream.Stop() })

			insock := filepath.Join(t.TempDir(), "in.sock")
			urlLine := ""
			if tc.url != "" {
				urlLine = "|    url: \"" + tc.url + "\"\n"
			}
			cnf := reindent(`
				|inlets.local:
				|    address: "` + insock + `"
				|    outlet: "upstream"
				|outlets.upstream:
				|    address: "` + upsock + `"
				` + urlLine + `
				|    tls-certs: ["ca1"]
				`)
			dir := filepath.Dir(insock)
			cfg, err := setup.Parse([]byte(cnf), dir)
			if err != nil {
				t.Fatalf("cannot parse setup: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			p, err := proxy.New(&proxy.ProxyOptions{Setup: cfg, TLSPath: tlsDir})
			if err != nil {
				t.Fatalf("cannot start proxy: %v", err)
			}
			p.Serve(ctx)

			client := harness.NewClient("unix:" + insock)
			resp, err := client.Do("GET", "/messages", nil, "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.Status != 200 || resp.Body != "hi" {
				t.Errorf("response = %d %q", resp.Status, resp.Body)
			}
			req := upstream.LastRequest()
			if req == nil {
				t.Fatalf("outlet received no request")
			}
			if req.Path != tc.path {
				t.Errorf("Path = %q, want %q", req.Path, tc.path)
			}
			if req.Host != "localhost" {
				t.Errorf("Host = %q, want localhost", req.Host)
			}
		})
	}
}

func TestProxyOutletNoURLTCP(t *testing.T) {
	// A tcp outlet with no url dials and names its own address:
	// the client's host stops riding the conversation.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "upstream"
		|outlets.upstream:
		|    address: "` + addrOf(upURL) + `"
		`)
	_ = startProxy(t, cnf)

	if _, err := harness.NewVhostClient("http://"+addr, "h.example.test").Do("GET", "/messages", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := upstream.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if req.Path != "/messages" {
		t.Errorf("Path = %q, want /messages", req.Path)
	}
	if req.Host != addrOf(upURL) {
		t.Errorf("Host = %q, want %q", req.Host, addrOf(upURL))
	}
}

func TestProxyOutletNoURLTCPTLS(t *testing.T) {
	// A tls outlet with no url speaks https to its own address:
	// the generated anchor matches it, so a plain client
	// request crosses - the host the client names never rides
	// the conversation.
	tlsDir := t.TempDir()
	store := rebeartls.NewStore(tlsDir)
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot listen on the upstream: %v", err)
	}
	upAddr := upLn.Addr().String()
	// The anchor is minted for the endpoint's own name.
	anchor, err := store.Pair("up", upAddr, true)
	if err != nil {
		t.Fatalf("cannot create anchor: %v", err)
	}
	upstream := harness.NewServer(harness.Response{Status: 200, Body: "ok"})
	upstreamCfg := &tls.Config{Certificates: []tls.Certificate{*anchor.Full}}
	if _, err := upstream.StartOn(rebeartls.NewListener(upLn, upstreamCfg)); err != nil {
		t.Fatalf("cannot start upstream: %v", err)
	}
	t.Cleanup(func() { _ = upstream.Stop() })

	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "up"
		|outlets.up:
		|    address: "` + upAddr + `"
		|    tls-certs: ["up"]
		`)
	_ = startTLSProxy(t, cnf, tlsDir)

	resp, err := harness.NewClient("http://"+addr).Do("GET", "/messages", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 || resp.Body != "ok" {
		t.Errorf("response = %d %q", resp.Status, resp.Body)
	}
	req := upstream.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if req.Host != upAddr {
		t.Errorf("Host = %q, want %q", req.Host, upAddr)
	}
}

func TestProxyOutletPathOnlyLocalTCP(t *testing.T) {
	// A path-only url on a local tcp outlet speaks http to the
	// address, and its host carries the spelled port.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "upstream"
		|outlets.upstream:
		|    address: "` + addrOf(upURL) + `"
		|    url: "/api"
		`)
	_ = startProxy(t, cnf)

	if _, err := harness.NewClient("http://"+addr).Do("GET", "/messages", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := upstream.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if req.Path != "/api/messages" {
		t.Errorf("Path = %q, want /api/messages", req.Path)
	}
	if req.Host != addrOf(upURL) {
		t.Errorf("Host = %q, want %q", req.Host, addrOf(upURL))
	}
}

func TestProxyVhostTCP(t *testing.T) {
	// A url host on a tcp inlet matches the request's Host, and a
	// request naming another host is a 404 the path aside.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.api:
		|    address: "` + addr + `"
		|    url: "http://api.example.com/api"
		|    outlet: "upstream"
		|outlets.upstream:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)

	client := harness.NewVhostClient("http://"+addr, "api.example.com")
	resp, err := client.Do("GET", "/api/x", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("Status = %d, want 200", resp.Status)
	}
	if req := upstream.LastRequest(); req == nil || req.Path != "/x" {
		t.Errorf("outlet path = %v, want /x", req)
	}

	resp, err = harness.NewVhostClient("http://"+addr, "other.example.com").Do("GET", "/api/x", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 404 {
		t.Errorf("Status = %d, want 404", resp.Status)
	}
	if want := "rebear: requested host is unknown: http://other.example.com/api/x"; resp.Body != want+"\n" {
		t.Errorf("Body = %q, want %q", resp.Body, want)
	}
}

func TestProxyVhostPortIncluded(t *testing.T) {
	// The vhost match keeps the port: a url naming one only
	// matches a Host that names it too.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.api:
		|    address: "` + addr + `"
		|    url: "http://api.example.com:8080"
		|    outlet: "upstream"
		|outlets.upstream:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)

	client := harness.NewVhostClient("http://"+addr, "api.example.com:8080")
	if _, err := client.Do("GET", "/x", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp, err := harness.NewVhostClient("http://"+addr, "api.example.com").Do("GET", "/x", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 404 {
		t.Errorf("Status = %d, want 404", resp.Status)
	}
}

func TestProxyVhostBeatsHostless(t *testing.T) {
	// On one address, a vhost-bearing inlet wins over a hostless
	// one with the same mount; requests naming no such host fall
	// through to the hostless one.
	vhost, vhostURL := startUpstream(t, harness.Response{Status: 200, Body: "vhost"})
	plain, plainURL := startUpstream(t, harness.Response{Status: 200, Body: "plain"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.vhost:
		|    address: "` + addr + `"
		|    url: "http://api.example.com/api"
		|    outlet: "vhost-up"
		|inlets.plain:
		|    address: "` + addr + `"
		|    url: "/api"
		|    outlet: "plain-up"
		|outlets.vhost-up:
		|    address: "` + addrOf(vhostURL) + `"
		|    url: "` + vhostURL + `"
		|outlets.plain-up:
		|    address: "` + addrOf(plainURL) + `"
		|    url: "` + plainURL + `"
		`)
	_ = startProxy(t, cnf)

	client := harness.NewVhostClient("http://"+addr, "api.example.com")
	resp, err := client.Do("GET", "/api/x", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != "vhost" {
		t.Errorf("response body = %q, want the vhost inlet's", resp.Body)
	}
	if req := vhost.LastRequest(); req == nil || req.Path != "/x" {
		t.Errorf("vhost outlet path = %v, want /x", req)
	}
	resp, err = harness.NewVhostClient("http://"+addr, "other.example.com").Do("GET", "/api/x", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != "plain" {
		t.Errorf("response body = %q, want the plain inlet's", resp.Body)
	}
	if req := plain.LastRequest(); req == nil || req.Path != "/x" {
		t.Errorf("plain outlet path = %v, want /x", req)
	}
}

func TestProxyOutletAddressSplit(t *testing.T) {
	// The outlet dials its address while the request speaks the
	// url's host: the connection and the name need not agree.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    url: "/v1"
		|    outlet: "up"
		|outlets.up:
		|    address: "` + addrOf(upURL) + `"
		|    url: "http://api.example.com/base"
		`)
	_ = startProxy(t, cnf)

	if _, err := harness.NewClient("http://"+addr).Do("GET", "/v1/x", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := upstream.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if req.Path != "/base/x" {
		t.Errorf("Path = %q, want /base/x", req.Path)
	}
	if req.Host != "api.example.com" {
		t.Errorf("Host = %q, want api.example.com", req.Host)
	}
}

func TestProxyUnmatchedMount(t *testing.T) {
	// A request outside the mount gets a 404 from rebear itself and
	// never reaches the outlets.
	// The mount is the listen URL's own path, cut before routing.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    url: "/v1"
		|    outlet: "upstream"
		|outlets.upstream:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)

	resp, err := harness.NewClient("http://"+addr).Do("GET", "/other", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 404 {
		t.Errorf("Status = %d, want 404", resp.Status)
	}
	if len(upstream.Requests()) != 0 {
		t.Errorf("outlet received %d requests, want 0", len(upstream.Requests()))
	}
}

func TestProxySharedListenerTCP(t *testing.T) {
	// Two inlets share one tcp address with nested mounts: the
	// deepest matching mount wins, and each inlet's outlet
	// receives the trailing path appended to its own URL.
	api, apiURL := startUpstream(t, harness.Response{Status: 200, Body: "api"})
	v2, v2URL := startUpstream(t, harness.Response{Status: 200, Body: "v2"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.api:
		|    address: "` + addr + `"
		|    url: "/api"
		|    outlet: "api-up"
		|inlets.v2:
		|    address: "` + addr + `"
		|    url: "/api/v2"
		|    outlet: "v2-up"
		|outlets.api-up:
		|    address: "` + addrOf(apiURL) + `"
		|    url: "` + apiURL + `"
		|outlets.v2-up:
		|    address: "` + addrOf(v2URL) + `"
		|    url: "` + v2URL + `"
		`)
	_ = startProxy(t, cnf)

	client := harness.NewClient("http://" + addr)
	resp, err := client.Do("GET", "/api/v2/x", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != "v2" {
		t.Errorf("response body = %q, want the v2 inlet's", resp.Body)
	}
	if req := v2.LastRequest(); req == nil || req.Path != "/x" {
		t.Errorf("v2 outlet path = %v, want /x", req)
	}
	resp, err = client.Do("GET", "/api/y", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != "api" {
		t.Errorf("response body = %q, want the api inlet's", resp.Body)
	}
	if req := api.LastRequest(); req == nil || req.Path != "/y" {
		t.Errorf("api outlet path = %v, want /y", req)
	}
}

func TestProxySharedListenerUnix(t *testing.T) {
	// The same sharing over one unix socket, the urls naming a
	// vhost each: the socket itself says nothing about the http
	// conversation over it, and the request's Host picks the
	// inlet.
	api, apiURL := startUpstream(t, harness.Response{Status: 200, Body: "api"})
	v2, v2URL := startUpstream(t, harness.Response{Status: 200, Body: "v2"})
	sock := filepath.Join(t.TempDir(), "shared.sock")
	cnf := reindent(`
		|inlets.api:
		|    address: "` + sock + `"
		|    url: "http://api.example.com/api"
		|    outlet: "api-up"
		|inlets.v2:
		|    address: "` + sock + `"
		|    url: "http://v2.example.com/api/v2"
		|    outlet: "v2-up"
		|outlets.api-up:
		|    address: "` + addrOf(apiURL) + `"
		|    url: "` + apiURL + `"
		|outlets.v2-up:
		|    address: "` + addrOf(v2URL) + `"
		|    url: "` + v2URL + `"
		`)
	_ = startProxy(t, cnf)

	// The vhost picks the inlet: the same path serves api under
	// api.example.com and v2 under v2.example.com.
	client := harness.NewVhostClient("unix:"+sock, "v2.example.com")
	resp, err := client.Do("GET", "/api/v2/x", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != "v2" {
		t.Errorf("response body = %q, want the v2 inlet's", resp.Body)
	}
	if req := v2.LastRequest(); req == nil || req.Path != "/x" {
		t.Errorf("v2 outlet path = %v, want /x", req)
	}
	client = harness.NewVhostClient("unix:"+sock, "api.example.com")
	resp, err = client.Do("GET", "/api/y", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != "api" {
		t.Errorf("response body = %q, want the api inlet's", resp.Body)
	}
	if req := api.LastRequest(); req == nil || req.Path != "/y" {
		t.Errorf("api outlet path = %v, want /y", req)
	}
}

func TestProxySharedListenerUnknownHost(t *testing.T) {
	// On a shared listener, a host none of the vhosts matches is
	// a 404 named unknown host, the full requested url in the
	// message: the path may exist under another host.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.one:
		|    address: "` + addr + `"
		|    url: "http://one.example.com/paths"
		|    outlet: "upstream"
		|inlets.two:
		|    address: "` + addr + `"
		|    url: "http://two.example.com/paths"
		|    outlet: "upstream"
		|outlets.upstream:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)

	client := harness.NewVhostClient("http://"+addr, "other.example.com")
	resp, err := client.Do("GET", "/paths/x", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 404 {
		t.Errorf("Status = %d, want 404", resp.Status)
	}
	if want := "rebear: requested host is unknown: http://other.example.com/paths/x"; resp.Body != want+"\n" {
		t.Errorf("Body = %q, want %q", resp.Body, want)
	}
}

func TestProxySharedListenerUnknownPath(t *testing.T) {
	// On a shared listener, a path no mount matches is a 404
	// logged with no flow: the host may be fine, the path is
	// not.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.one:
		|    address: "` + addr + `"
		|    url: "/one"
		|    outlet: "upstream"
		|inlets.two:
		|    address: "` + addr + `"
		|    url: "/two"
		|    outlet: "upstream"
		|outlets.upstream:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	log := captureLog(t)
	if err := startProxyLog(t, cnf); err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/other", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 404 {
		t.Errorf("Status = %d, want 404", resp.Status)
	}
	if want := "rebear: requested path is unknown: http://" + addr + "/other"; resp.Body != want+"\n" {
		t.Errorf("Body = %q, want %q", resp.Body, want)
	}
	got := logEntries(t, log)
	// The two listener-started lines aside, exactly one record:
	// the 404 with no flow - the host fits, the path does not.
	var reqs []map[string]any
	for _, m := range got {
		if m["method"] != nil {
			reqs = append(reqs, m)
		}
	}
	want := map[string]any{"method": "GET", "path": "/other", "code": float64(404), "error": "requested path is unknown: http://" + addr + "/other"}
	if len(reqs) != 1 || !hasEntry(reqs, want) || reqs[0]["flow"] != nil {
		t.Errorf("request records = %q, want one holding %q with no flow", reqs, want)
	}
}

func TestProxyDuplicateMount(t *testing.T) {
	// Two inlets with the exact same mount on one address are
	// ambiguous and refused at startup.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.one:
		|    address: "` + addr + `"
		|    url: "/v1"
		|    outlet: "upstream"
		|inlets.two:
		|    address: "` + addr + `"
		|    url: "/v1"
		|    outlet: "upstream"
		|outlets.upstream:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	err := startProxyLog(t, cnf)
	if err == nil {
		t.Fatalf("duplicate mount accepted")
	}
	// Parse ranges over a map, so either inlet may be named the
	// duplicate; both orderings name both inlets and the url.
	msg := `inlet "two": url "/v1" already served by inlet "one"`
	alt := `inlet "one": url "/v1" already served by inlet "two"`
	if !strings.Contains(err.Error(), msg) && !strings.Contains(err.Error(), alt) {
		t.Errorf("error = %q, want a duplicate mount complaint", err.Error())
	}
}

func TestProxyBinaryBody(t *testing.T) {
	// A non-JSON body round-trips byte for byte, both ways: the
	// no-filtering invariant, pinned before filters exist.
	body := "\x1f\x8braw\x00bytes\xff"
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: body})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "upstream"
		|outlets.upstream:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)

	resp, err := harness.NewClient("http://"+addr).Do("PUT", "/upload", nil, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != body {
		t.Errorf("response body bytes differ: %q", resp.Body)
	}
	if upstream.LastRequest().Body != body {
		t.Errorf("request body bytes differ: %q", upstream.LastRequest().Body)
	}
}

func TestProxyStaleSocket(t *testing.T) {
	// A leftover socket file from a killed run is removed before
	// binding, so the inlet comes up on a fresh one.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	if err := os.WriteFile(sock, []byte("stale"), 0o600); err != nil {
		t.Fatalf("cannot write stale socket: %v", err)
	}
	cnf := reindent(`
		|inlets.local:
		|    address: "` + sock + `"
		|    outlet: "upstream"
		|outlets.upstream:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)

	if _, err := harness.NewClient("unix:"+sock).Do("GET", "/probe", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req := upstream.LastRequest(); req != nil && req.Path != "/probe" {
		t.Errorf("Path = %q, want /probe", req.Path)
	}
}

func TestProxyCancelClosesListeners(t *testing.T) {
	// Cancelling the context stops serving; further requests fail
	// cleanly instead of hanging.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	dir := t.TempDir()
	sock := filepath.Join(dir, "cancel.sock")
	cnf := reindent(`
		|inlets.local:
		|    address: "` + sock + `"
		|    outlet: "upstream"
		|outlets.upstream:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	cfg, err := setup.Parse([]byte(cnf), dir)
	if err != nil {
		t.Fatalf("cannot parse setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p, err := proxy.New(&proxy.ProxyOptions{Setup: cfg})
	if err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	p.Serve(ctx)
	cancel()
	_ = p.Stop()

	if _, err := harness.NewClient("unix:"+sock).Do("GET", "/", nil, ""); err == nil {
		t.Errorf("error = nil, want a failure after cancel")
	}
}

func TestProxyOpeningFlowPrints(t *testing.T) {
	// A request through a pair with opening-flow snippets runs
	// them, print landing on the logger in pair order, with the
	// dispatch line between them and the response.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    opening-flow: "print(\"from inlet\")"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		|    opening-flow: "print(\"from outlet\")"
		`)
	log := captureLog(t)
	if err := startProxyLog(t, cnf); err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	if _, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if upstream.LastRequest() == nil {
		t.Fatalf("outlet received no request")
	}
	got := logEntries(t, log)
	want := []map[string]any{{
		"log": []any{map[string]any{
			"msg":     "started listening",
			"inlet":   "local",
			"address": addr,
		}},
	}, {
		// The start record: the inlet's flow, method and path -
		// nothing run yet.
		"method": "GET", "path": "/", "flow": []any{"local"},
	}, {
		// The final record: the prints ride its log list, one
		// object per call, in pair order.
		"method": "GET",
		"path":   "/",
		"flow":   []any{"local", "remote"},
		"code":   float64(200),
		"log": []any{
			map[string]any{"at": "inlets.local.opening-flow:1", "print": "from inlet"},
			map[string]any{"at": "outlets.remote.opening-flow:1", "print": "from outlet"},
		},
	}}
	if len(got) != len(want) {
		t.Fatalf("log = %q, want %q", got, want)
	}
	for _, w := range want {
		if !hasEntry(got, w) {
			t.Errorf("log %q, want it to contain %q", got, w)
		}
	}
}

func TestProxyLogsDispatch(t *testing.T) {
	// Each request logs its route and method after the outlet
	// settles: the plain form when the URL is unchanged, the
	// rewrite form when the mount cut or outlet path moved it.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.glm:
		|    address: "` + addr + `"
		|    url: "/v1"
		|    outlet: "router"
		|outlets.router:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `/api"
		`)
	log := captureLog(t)
	if err := startProxyLog(t, cnf); err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	if _, err := harness.NewClient("http://"+addr).Do("GET", "/v1/foo/bar", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]any{
		"flow":     []any{"glm", "router"},
		"method":   "GET",
		"path":     "/v1/foo/bar",
		"out-path": "/api/foo/bar",
	}
	if !hasEntry(logEntries(t, log), want) {
		t.Errorf("log = %q, want it to contain %q", logEntries(t, log), want)
	}
}

func TestProxyLogsFail(t *testing.T) {
	// A flow that fails logs its route with the failure;
	// a fail() code appears in the message.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-headers: 'fail("slow down", code=429)'
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	log := captureLog(t)
	if err := startProxyLog(t, cnf); err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	if _, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The fail record carries the route and the failure: the
	// message and the fail() code as the status code the client
	// saw, no snippet position - LogAt off is the default.
	want := map[string]any{
		"flow":   []any{"local", "remote"},
		"method": "GET",
		"path":   "/",
		"code":   float64(429),
		"error":  "slow down (429)",
	}
	if !hasEntry(logEntries(t, log), want) {
		t.Errorf("log = %q, want it to contain %q", logEntries(t, log), want)
	}
}

func TestProxyLogsFailAtLogAt(t *testing.T) {
	// LogAt on: the fail record's error gains the snippet's
	// config path and position, no function name.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-headers: 'fail("slow down", code=429)'
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	log := captureLog(t)
	dir := t.TempDir()
	cfg, err := setup.Parse([]byte(cnf), dir)
	if err != nil {
		t.Fatalf("cannot parse setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p, err := proxy.New(&proxy.ProxyOptions{Setup: cfg, LogAt: true})
	if err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	p.Serve(ctx)
	t.Cleanup(func() { _ = p.Stop() })
	if _, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]any{
		"flow":   []any{"local", "remote"},
		"method": "GET",
		"path":   "/",
		"code":   float64(429),
		"error":  "inlets.local.outgoing-headers:1:5: slow down (429)",
	}
	if !hasEntry(logEntries(t, log), want) {
		t.Errorf("log = %q, want it to contain %q", logEntries(t, log), want)
	}
}

func TestProxyLogsLogCalls(t *testing.T) {
	// A snippet's log() calls land on the request record, under
	// log: one object per call, each with its at position once
	// LogAt is on - the default keeps the objects bare. The
	// start record holds none - nothing ran yet.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    opening-flow: "log({\"key\": \"value\"})"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		|    opening-flow: "log({\"other\": 1})"
		`)
	log := captureLog(t)
	dir := t.TempDir()
	cfg, err := setup.Parse([]byte(cnf), dir)
	if err != nil {
		t.Fatalf("cannot parse setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p, err := proxy.New(&proxy.ProxyOptions{Setup: cfg, LogAt: true})
	if err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	p.Serve(ctx)
	t.Cleanup(func() { _ = p.Stop() })
	if _, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if upstream.LastRequest() == nil {
		t.Fatalf("outlet received no request")
	}
	want := map[string]any{
		"flow":   []any{"local", "remote"},
		"method": "GET",
		"path":   "/",
		"log": []any{
			map[string]any{"at": "inlets.local.opening-flow:1", "key": "value"},
			map[string]any{"at": "outlets.remote.opening-flow:1", "other": float64(1)},
		},
	}
	if !hasEntry(logEntries(t, log), want) {
		t.Errorf("log = %q, want it to contain %q", logEntries(t, log), want)
	}
}

func TestProxyClosingFlowLogs(t *testing.T) {
	// The closings run once the whole flow has finished, after
	// the response reached the client: their log() calls still
	// land in the final record, in reverse opening order.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    closing-flow: "log({\"key\": \"value\"})"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		|    closing-flow: "log({\"other\": 1})"
		`)
	log := captureLog(t)
	dir := t.TempDir()
	cfg, err := setup.Parse([]byte(cnf), dir)
	if err != nil {
		t.Fatalf("cannot parse setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p, err := proxy.New(&proxy.ProxyOptions{Setup: cfg, LogAt: true})
	if err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	p.Serve(ctx)
	t.Cleanup(func() { _ = p.Stop() })
	if _, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if upstream.LastRequest() == nil {
		t.Fatalf("outlet received no request")
	}
	want := map[string]any{
		"flow":   []any{"local", "remote"},
		"method": "GET",
		"path":   "/",
		"code":   float64(200),
		"log": []any{
			map[string]any{"at": "outlets.remote.closing-flow:1", "other": float64(1)},
			map[string]any{"at": "inlets.local.closing-flow:1", "key": "value"},
		},
	}
	if !hasEntry(logEntries(t, log), want) {
		t.Errorf("log = %q, want it to contain %q", logEntries(t, log), want)
	}
}

func TestProxyBrokenSnippetFailsStartup(t *testing.T) {
	// A snippet that does not compile is a startup error,
	// naming the pair and the position.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    opening-flow: "def(:="
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	dir := t.TempDir()
	cfg, err := setup.Parse([]byte(cnf), dir)
	if err != nil {
		t.Fatalf("cannot parse setup: %v", err)
	}
	_, err = proxy.New(&proxy.ProxyOptions{Setup: cfg})
	if err == nil || !strings.Contains(err.Error(), "inlets.local.opening-flow:1:5:") {
		t.Fatalf("error = %v, want the snippet path and a position", err)
	}
}

func TestProxyOpeningFlowFailureFailsFlow(t *testing.T) {
	// A runtime failure in an opening-flow snippet ends the
	// flow: the client
	// gets the failure, the outlet receives nothing.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    opening-flow: "x = 1 // 0"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	if err := startProxyLog(t, cnf); err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 500 {
		t.Errorf("Status = %d, want 500", resp.Status)
	}
	if !strings.Contains(resp.Body, "division by zero") {
		t.Errorf("Body = %q, want the starlark failure", resp.Body)
	}
	if n := len(upstream.Requests()); n != 0 {
		t.Errorf("outlet received %d requests, want 0", n)
	}
}

func TestProxyNoOutletSelected(t *testing.T) {
	// No outlet is selected by name match: with no outlet field
	// on the inlet or a midlet and no set_outlet call - even
	// with a same-name outlet present - the request fails with
	// 502 and reaches no outlet.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|outlets.local:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	log := captureLog(t)
	if err := startProxyLog(t, cnf); err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 502 {
		t.Errorf("Status = %d, want 502", resp.Status)
	}
	if !strings.HasPrefix(resp.Body, "rebear: no outlet selected") {
		t.Errorf("Body = %q, want the rebear-prefixed no outlet failure", resp.Body)
	}
	want := map[string]any{
		"flow":   []any{"local"},
		"method": "GET",
		"path":   "/",
		"code":   float64(502),
		"error":  "no outlet selected",
	}
	if !hasEntry(logEntries(t, log), want) {
		t.Errorf("log = %q, want it to contain %q", logEntries(t, log), want)
	}
	if n := len(upstream.Requests()); n != 0 {
		t.Errorf("outlet received %d requests, want 0", n)
	}
}

// startProxySnapshots starts a proxy on cnf with its snapshots
// landing in a per-test directory, answering the directory.
func startProxySnapshots(t *testing.T, cnf string) string {
	t.Helper()
	dir := t.TempDir()
	snaps := filepath.Join(dir, "snapshots")
	cfg, err := setup.Parse([]byte(cnf), dir)
	if err != nil {
		t.Fatalf("cannot parse setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p, err := proxy.New(&proxy.ProxyOptions{Setup: cfg, SnapshotsPath: snaps})
	if err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	p.Serve(ctx)
	t.Cleanup(func() { _ = p.Stop() })
	return snaps
}

// startProxyLog starts a proxy on cnf, with both package
// loggers captured, reporting the startup error instead of
// failing.
func startProxyLog(t *testing.T, cnf string) error {
	t.Helper()
	dir := t.TempDir()
	cfg, err := setup.Parse([]byte(cnf), dir)
	if err != nil {
		t.Fatalf("cannot parse setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p, err := proxy.New(&proxy.ProxyOptions{Setup: cfg})
	if err != nil {
		return err
	}
	p.Serve(ctx)
	t.Cleanup(func() { _ = p.Stop() })
	return nil
}

// captureLog routes the JSONL log stream into one buffer,
// restored when the test ends.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	logging.SetWriter(buf)
	t.Cleanup(func() { logging.SetWriter(nil) })
	return buf
}

// logEntries decodes each captured JSONL line to a map,
// dropping the time key.
func logEntries(t *testing.T, buf *syncBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range logLines(buf) {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("cannot decode log line %q: %v", line, err)
		}
		delete(m, "time")
		out = append(out, m)
	}
	return out
}

// hasEntry reports whether one decoded line holds every key of
// want with an equal value.
func hasEntry(got []map[string]any, want map[string]any) bool {
	for _, m := range got {
		matches := true
		for k, v := range want {
			if !reflect.DeepEqual(m[k], v) {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

// logLines returns the buffer's contents split into lines,
// dropping the trailing empty one.
func logLines(buf *syncBuffer) []string {
	return strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
}

// syncBuffer is a strings.Builder safe to read while another
// goroutine writes.
type syncBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func TestProxyOutgoingHeadersFiltered(t *testing.T) {
	// Request headers pass the pair's outgoing filters before
	// forwarding: additions and deletions both reach the
	// outlet.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-headers:
		|        """
		|        headers.pop("X-Drop", None)
		|        headers["X-Add"] = "yes"
		|        """
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	if _, err := harness.NewClient("http://"+addr).Do("GET", "/", map[string]string{"X-Drop": "bye"}, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := upstream.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if _, ok := req.Headers["X-Drop"]; ok {
		t.Errorf("X-Drop = %q, want deleted", req.Headers["X-Drop"])
	}
	if req.Headers["X-Add"] != "yes" {
		t.Errorf("X-Add = %q, want yes", req.Headers["X-Add"])
	}
}

func TestProxyIncomingHeadersFiltered(t *testing.T) {
	// Response headers pass the pair's incoming filters
	// before writing back: deletions reach the client.
	_, upURL := startUpstream(t, harness.Response{
		Status:  200,
		Headers: map[string]string{"X-Junk": "bye", "X-Keep": "yes"},
	})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    incoming-headers: "headers.pop(\"X-Junk\", None)"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := resp.Headers["X-Junk"]; ok {
		t.Errorf("X-Junk = %q, want deleted", resp.Headers["X-Junk"])
	}
	if resp.Headers["X-Keep"] != "yes" {
		t.Errorf("X-Keep = %q, want yes", resp.Headers["X-Keep"])
	}
}

func TestProxyHeaderFilterFailureFailsFlow(t *testing.T) {
	// A failing outgoing filter ends the flow before any
	// forwarding.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-headers: 'x = 1 // 0'
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 500 {
		t.Errorf("Status = %d, want 500", resp.Status)
	}
	if n := len(upstream.Requests()); n != 0 {
		t.Errorf("outlet received %d requests, want 0", n)
	}
}

func TestProxyFailReachesClient(t *testing.T) {
	// A filter's fail(code=..., body=...) rejection answers
	// the client with that status and body, and the outlet
	// receives nothing.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-headers: 'fail("slow down", code=429, body={"error": "rate limited", "retry": False})'
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 429 {
		t.Errorf("Status = %d, want 429", resp.Status)
	}
	want := `{"error":"rate limited","retry":false}`
	if resp.Body != want {
		t.Errorf("Body = %q, want %q", resp.Body, want)
	}
	if resp.Headers["Content-Type"] != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", resp.Headers["Content-Type"])
	}
}

func TestProxyFailNoCodeIs500(t *testing.T) {
	// A fail without code answers 500 with the message as the
	// body, plain text, no content type override.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    opening-flow: "fail(\"broken setup\")"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 500 {
		t.Errorf("Status = %d, want 500", resp.Status)
	}
	if resp.Body != "broken setup" {
		t.Errorf("Body = %q, want the fail message", resp.Body)
	}
	// A plain text body sets no header; net/http sniffs it.
	if ct := resp.Headers["Content-Type"]; ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the sniffed text/plain", ct)
	}
}

func TestProxyMidletChain(t *testing.T) {
	// A request runs the inlet, the midlet chain, and the outlet,
	// in that order; the response walks the midlets back in
	// reverse, between the outlet's and the inlet's filters.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Headers: map[string]string{"X-Order": "outlet-in"}, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    midlets: ["one"]
		|    outgoing-headers: 'headers["X-Order"] = "inlet"'
		|    incoming-headers: 'headers["X-Order"] += ",inlet-in"'
		|midlets.one:
		|    midlets: ["two"]
		|    outgoing-headers: 'headers["X-Order"] += ",one"'
		|    incoming-headers: "headers[\"X-Order\"] += \",one-in\""
		|midlets.two:
		|    outgoing-headers: 'headers["X-Order"] += ",two"'
		|    incoming-headers: "headers[\"X-Order\"] += \",two-in\""
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	log := captureLog(t)
	if err := startProxyLog(t, cnf); err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := upstream.LastRequest().Headers["X-Order"]; got != "inlet,one,two" {
		t.Errorf("request X-Order = %q, want inlet,one,two", got)
	}
	if got := resp.Headers["X-Order"]; got != "outlet-in,two-in,one-in,inlet-in" {
		t.Errorf("response X-Order = %q, want outlet-in,two-in,one-in,inlet-in", got)
	}
	// The request record names the whole chain.
	want := map[string]any{
		"flow":   []any{"local", "one", "two", "remote"},
		"method": "GET",
		"path":   "/",
	}
	if !hasEntry(logEntries(t, log), want) {
		t.Errorf("log = %q, want it to contain %q", logEntries(t, log), want)
	}
}

func TestProxyMidletOverridesOutlet(t *testing.T) {
	// A midlet's static outlet overrides the inlet's config
	// choice: the request leaves through the midlet's outlet.
	primary, primaryURL := startUpstream(t, harness.Response{Status: 200, Body: "primary"})
	backup, backupURL := startUpstream(t, harness.Response{Status: 200, Body: "backup"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    midlets: ["pick"]
		|midlets.pick:
		|    outlet: "backup"
		|outlets.primary:
		|    address: "` + addrOf(primaryURL) + `"
		|    url: "` + primaryURL + `"
		|outlets.backup:
		|    address: "` + addrOf(backupURL) + `"
		|    url: "` + backupURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != "backup" {
		t.Errorf("Body = %q, want the backup outlet's reply", resp.Body)
	}
	if n := len(primary.Requests()); n != 0 {
		t.Errorf("primary received %d requests, want 0", n)
	}
	if backup.LastRequest() == nil {
		t.Fatalf("backup received no request")
	}
}

func TestProxyAddMidletFromInlet(t *testing.T) {
	// add_midlet from the inlet's snippet appends behind the
	// config list: the config midlet runs first, the recorded
	// one after it.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    midlets: ["one"]
		|    outgoing-headers: 'flow.add_midlet("two")'
		|midlets.one:
		|    outgoing-headers: 'headers["X-Mark"] = "one"'
		|midlets.two:
		|    outgoing-headers: 'headers["X-Mark"] = "two"'
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	log := captureLog(t)
	if err := startProxyLog(t, cnf); err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	if _, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := upstream.LastRequest().Headers["X-Mark"]; got != "two" {
		t.Errorf("X-Mark = %q, want two", got)
	}
	want := map[string]any{"flow": []any{"local", "one", "two", "remote"}}
	if !hasEntry(logEntries(t, log), want) {
		t.Errorf("log = %q, want it to contain %q", logEntries(t, log), want)
	}
}

func TestProxyMidletRunsOnce(t *testing.T) {
	// A midlet reached a second time is skipped: the inlet's
	// list and a midlet's own listing of the same name run it
	// once, and a mutual pair cannot loop the chain.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    midlets: ["one", "two"]
		|midlets.one:
		|    midlets: ["two"]
		|    outgoing-headers: 'headers["X-Order"] = "one"'
		|midlets.two:
		|    midlets: ["one"]
		|    outgoing-headers: 'headers["X-Order"] += ",two"'
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	log := captureLog(t)
	if err := startProxyLog(t, cnf); err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("Status = %d, want 200", resp.Status)
	}
	if got := upstream.LastRequest().Headers["X-Order"]; got != "one,two" {
		t.Errorf("X-Order = %q, want one,two", got)
	}
	want := map[string]any{"flow": []any{"local", "one", "two", "remote"}}
	if !hasEntry(logEntries(t, log), want) {
		t.Errorf("log = %q, want it to contain %q", logEntries(t, log), want)
	}
}

func TestProxyMidletQueueInterleaving(t *testing.T) {
	// The chain interleaves config lists and add_midlet records
	// through one queue, each name running once, in first-listing
	// order: the inlet's list seeds the queue, its opening appends
	// behind, and each popped midlet's list and records join the
	// end the same way.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    midlets: ["a", "b"]
		|    opening-flow:
		|        """
		|        flow.add_midlet("a")
		|        flow.add_midlet("c")
		|        flow.add_midlet("b")
		|        flow.add_midlet("d")
		|        """
		|    outgoing-headers: 'headers["X-Order"] = "inlet"'
		|midlets.a:
		|    midlets: ["a", "e", "c", "f", "b"]
		|    opening-flow:
		|        """
		|        flow.add_midlet("b")
		|        flow.add_midlet("g")
		|        flow.add_midlet("f")
		|        flow.add_midlet("h")
		|        """
		|    outgoing-headers: 'headers["X-Order"] += ",a"'
		|midlets.b:
		|    outgoing-headers: 'headers["X-Order"] += ",b"'
		|midlets.c:
		|    outgoing-headers: 'headers["X-Order"] += ",c"'
		|midlets.d:
		|    outgoing-headers: 'headers["X-Order"] += ",d"'
		|midlets.e:
		|    outgoing-headers: 'headers["X-Order"] += ",e"'
		|midlets.f:
		|    outgoing-headers: 'headers["X-Order"] += ",f"'
		|midlets.g:
		|    outgoing-headers: 'headers["X-Order"] += ",g"'
		|midlets.h:
		|    outgoing-headers: 'headers["X-Order"] += ",h"'
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	log := captureLog(t)
	if err := startProxyLog(t, cnf); err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("Status = %d, want 200", resp.Status)
	}
	if got := upstream.LastRequest().Headers["X-Order"]; got != "inlet,a,b,c,d,e,f,g,h" {
		t.Errorf("X-Order = %q, want inlet,a,b,c,d,e,f,g,h", got)
	}
	want := map[string]any{"flow": []any{"local", "a", "b", "c", "d", "e", "f", "g", "h", "remote"}}
	if !hasEntry(logEntries(t, log), want) {
		t.Errorf("log = %q, want it to contain %q", logEntries(t, log), want)
	}
}

func TestProxyMidletFiltersBody(t *testing.T) {
	// A midlet's outgoing-body runs on the buffered request
	// body, and its incoming-body on the response body.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: `{"secret":"s3cr3t","n":1}`})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "sample"
		|    midlets: ["strip"]
		|midlets.strip:
		|    outgoing-body: "body.json.pop(\"pin\")"
		|    incoming-body: "body.json.pop(\"secret\")"
		|outlets.sample:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("POST", "/", nil, `{"n":1,"pin":1234}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := upstream.LastRequest().Body; got != `{"n":1}` {
		t.Errorf("request body = %q, want the pin gone", got)
	}
	if got := resp.Body; got != `{"n":1}` {
		t.Errorf("response body = %q, want the secret gone", got)
	}
}

func TestProxySetOutletReroutes(t *testing.T) {
	// A snippet settles the outlet during the header stage:
	// the request leaves through the recorded outlet, not the
	// config's default.
	primary, primaryURL := startUpstream(t, harness.Response{Status: 200, Body: "primary"})
	backup, backupURL := startUpstream(t, harness.Response{Status: 200, Body: "backup"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "primary"
		|    outgoing-headers: 'flow.set_outlet("backup")'
		|outlets.primary:
		|    address: "` + addrOf(primaryURL) + `"
		|    url: "` + primaryURL + `"
		|outlets.backup:
		|    address: "` + addrOf(backupURL) + `"
		|    url: "` + backupURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != "backup" {
		t.Errorf("Body = %q, want the backup outlet's reply", resp.Body)
	}
	if n := len(primary.Requests()); n != 0 {
		t.Errorf("primary received %d requests, want 0", n)
	}
	if backup.LastRequest() == nil {
		t.Fatalf("backup received no request")
	}
}

func TestProxyFailStringBody(t *testing.T) {
	// An explicit string body overrides the message: the
	// client receives it verbatim, status from code=, and
	// the content type stays sniffed text.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-headers: 'fail("gone", code=410, body="see https://example.test/gone")'
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 410 {
		t.Errorf("Status = %d, want 410", resp.Status)
	}
	if resp.Body != "see https://example.test/gone" {
		t.Errorf("Body = %q, want the body string, not the message", resp.Body)
	}
	if ct := resp.Headers["Content-Type"]; ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the sniffed text/plain", ct)
	}
}

func TestProxyFailOverLimitJSONRequestFailsFlow(t *testing.T) {
	// A request body over the JSON limit ends the flow
	// before forwarding: a 500 with the sizes, and the outlet
	// receives nothing.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-body: "flow.state[\"seen\"] = \"yes\""
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	body := strings.Repeat("a", snippets.JSONLimit+2)
	resp, err := harness.NewClient("http://"+addr).Do("POST", "/", nil, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 500 {
		t.Errorf("Status = %d, want 500", resp.Status)
	}
	want := "exceeds the " + strconv.Itoa(snippets.JSONLimit) + " byte limit"
	if !strings.Contains(resp.Body, want) {
		t.Errorf("Body = %q, want it to name the limit", resp.Body)
	}
	if n := len(upstream.Requests()); n != 0 {
		t.Errorf("outlet received %d requests, want 0", n)
	}
}

func TestProxyOverLimitBodyPassesFilterlessRoute(t *testing.T) {
	// The buffering is lazy: an outlet with outgoing-body exists in
	// the setup, but the request's route holds no body filter, so an
	// oversized body streams through untouched instead of failing.
	body := strings.Repeat("a", snippets.JSONLimit+2)
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	filtering, filterURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		|outlets.filtering:
		|    address: "` + addrOf(filterURL) + `"
		|    url: "` + filterURL + `"
		|    outgoing-body: "flow.state[\"seen\"] = \"yes\""
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("POST", "/", nil, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("Status = %d, want 200", resp.Status)
	}
	if got := upstream.LastRequest().Body; got != body {
		t.Errorf("request body of %d bytes, want it to pass through", len(got))
	}
	if n := len(filtering.Requests()); n != 0 {
		t.Errorf("filtering outlet received %d requests, want 0", n)
	}
}

func TestProxyOutgoingBodyFiltered(t *testing.T) {
	// A request JSON body passes the pair's filters before
	// forwarding: deletions and additions both reach the
	// outlet, re-encoded.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-body: "body.json.pop(\"secret\", None)\nbody.json[\"added\"] = \"yes\""
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	if _, err := harness.NewClient("http://"+addr).Do("POST", "/", nil, `{"secret":"shh","keep":"yes"}`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := upstream.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if want := `{"keep":"yes","added":"yes"}`; req.Body != want {
		t.Errorf("request body = %q, want %q", req.Body, want)
	}
}

func TestProxyBodyContentType(t *testing.T) {
	// outgoing.type and incoming.type read the cleaned Content-Type
	// as the header stages left it: an outgoing-headers rewrite is
	// visible to the following outgoing-body, and the incoming side
	// sees the response's media type, parameters dropped.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Headers: map[string]string{"Content-Type": "Application/JSON; charset=utf-8"}, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-headers: 'headers["Content-Type"] = "Application/JSON; charset=utf-8"'
		|    outgoing-body: "if outgoing.type != \"application/json\":\n    fail(outgoing.type)"
		|    incoming-body: "if incoming.type != \"application/json\":\n    fail(incoming.type)"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("POST", "/", nil, `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("Status = %d, want 200: %q", resp.Status, resp.Body)
	}
}

func TestProxyOutgoingRewrites(t *testing.T) {
	// The method, the path, and the content type assign on the
	// way out: the outlet receives the rewritten request - the
	// assigned trailing path still prefixed with its url path,
	// which no snippet knows - and the log joins out-method only
	// on the divergence.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    url: "/v1"
		|    outlet: "remote"
		|    outgoing-headers:
		|        """
		|        outgoing.method = "PUT"
		|        outgoing.path = "/rewritten"
		|        """
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `/api"
		`)
	log := captureLog(t)
	if err := startProxyLog(t, cnf); err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	if _, err := harness.NewClient("http://"+addr).Do("POST", "/v1/x", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := upstream.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if req.Method != "PUT" {
		t.Errorf("Method = %q, want the rewritten PUT", req.Method)
	}
	// An assigned path replaces the trailing; the outlet's
	// prefix still lands on it at the send.
	if req.Path != "/api/rewritten" {
		t.Errorf("Path = %q, want /api/rewritten", req.Path)
	}
	want := map[string]any{
		"method":     "POST",
		"out-method": "PUT",
		"path":       "/v1/x",
		"out-path":   "/api/rewritten",
	}
	if !hasEntry(logEntries(t, log), want) {
		t.Errorf("log = %q, want it to contain %q", logEntries(t, log), want)
	}
}

func TestProxyQueryPassthrough(t *testing.T) {
	// An untouched query rides the wire verbatim: the odd
	// order, the repeated keys, the empty value - a snippet
	// reading it changes nothing.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    url: "/v1"
		|    outlet: "remote"
		|    outgoing-headers:
		|        """
		|        if outgoing.query["a"] != "1":
		|            fail("query not seeded")
		|        """
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `/api"
		`)
	log := captureLog(t)
	if err := startProxyLog(t, cnf); err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	if _, err := harness.NewClient("http://"+addr).Do("GET", "/v1/x?b=&a=1&a=2&b", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := upstream.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if req.Query != "b=&a=1&a=2&b" {
		t.Errorf("Query = %q, want the verbatim b=&a=1&a=2&b", req.Query)
	}
	// The query rides the start record too, and no out-query
	// joins: nothing diverged.
	want := map[string]any{
		"path":  "/v1/x",
		"query": "b=&a=1&a=2&b",
	}
	entries := logEntries(t, log)
	if !hasEntry(entries, want) {
		t.Errorf("log = %q, want it to contain %q", entries, want)
	}
	for _, entry := range entries {
		if _, ok := entry["out-query"]; ok {
			t.Errorf("log = %q, want no out-query on the passthrough", entry)
		}
	}
}

func TestProxyQueryRewrite(t *testing.T) {
	// A touched query re-encodes: the outlet receives the
	// canonical form, and the log joins out-query on the
	// divergence.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    url: "/v1"
		|    outlet: "remote"
		|    outgoing-headers:
		|        """
		|        outgoing.query["a"] = "3"
		|        outgoing.query.pop("b")
		|        """
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `/api"
		`)
	log := captureLog(t)
	if err := startProxyLog(t, cnf); err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	if _, err := harness.NewClient("http://"+addr).Do("GET", "/v1/x?b=2&a=1", nil, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := upstream.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if req.Query != "a=3" {
		t.Errorf("Query = %q, want the re-encoded a=3", req.Query)
	}
	want := map[string]any{
		"query":     "b=2&a=1",
		"out-query": "a=3",
	}
	if !hasEntry(logEntries(t, log), want) {
		t.Errorf("log = %q, want it to contain %q", logEntries(t, log), want)
	}
}

func TestProxyIncomingCodeRewrite(t *testing.T) {
	// The status code assigns on the way back: the client sees
	// the rewritten code, and the log joins in-code only on the
	// divergence.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    incoming-headers: "incoming.code = 201"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	log := captureLog(t)
	if err := startProxyLog(t, cnf); err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 201 {
		t.Errorf("Status = %d, want the rewritten 201", resp.Status)
	}
	want := map[string]any{
		"code":    float64(201),
		"in-code": float64(200),
	}
	if !hasEntry(logEntries(t, log), want) {
		t.Errorf("log = %q, want it to contain %q", logEntries(t, log), want)
	}
}

func TestProxyLaneFlipsOnHeaderRewrite(t *testing.T) {
	// The lane decision reads the headers as the incoming-headers
	// stages left them: a snippet flipping the content type to an
	// event stream moves a plain response into the event lane.
	chunks := []string{"data: {\"n\":1}\n\n"}
	_, upURL := startUpstream(t, harness.Response{Status: 200, Headers: map[string]string{"Content-Type": "application/json"}, Chunks: chunks})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    incoming-headers: "headers[\"Content-Type\"] = \"text/event-stream\""
		|    incoming-event: "event.json[\"x\"] = 1"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The event lane ran: the event's payload was filtered.
	if want := "data: {\"n\":1,\"x\":1}\n\n"; resp.Body != want {
		t.Errorf("response body = %q, want %q", resp.Body, want)
	}
}

func TestProxyIncomingBodyFiltered(t *testing.T) {
	// A response JSON body passes the pair's filters before
	// writing back: the client sees the filtered body.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: `{"model":"m","logprobs":"noisy"}`})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    incoming-body: "body.json.pop(\"logprobs\", None)"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := `{"model":"m"}`; resp.Body != want {
		t.Errorf("response body = %q, want %q", resp.Body, want)
	}
}

func TestProxyJSONIgnoresNonJSON(t *testing.T) {
	// A non-JSON body still reaches the filters - body.json reads
	// None - and round-trips byte for byte, both ways, unless the
	// snippet assigns.
	body := "\x1f\x8braw\x00bytes\xff"
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: body})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-body: "if body.json != None:\n    body.json[\"never\"] = \"run\""
		|    incoming-body: "if body.json != None:\n    body.json[\"never\"] = \"run\""
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("PUT", "/upload", nil, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != body {
		t.Errorf("response body bytes differ: %q", resp.Body)
	}
	if upstream.LastRequest().Body != body {
		t.Errorf("request body bytes differ: %q", upstream.LastRequest().Body)
	}
}

func TestProxyJSONListBodyEndToEnd(t *testing.T) {
	// A non-dict body crosses the whole path: a list request
	// mutates through the filter and reaches the outlet.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-body: "body.json.append(\"x\")"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	if _, err := harness.NewClient("http://"+addr).Do("POST", "/", nil, `[1,2]`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := upstream.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if want := `[1,2,"x"]`; req.Body != want {
		t.Errorf("request body = %q, want %q", req.Body, want)
	}
}

func TestProxyJSONReplacesRequestBody(t *testing.T) {
	// An assignment to body.json replaces the body end to end: a
	// string request is wrapped into an object before the outlet.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-body: "body.json = {\"body\": body.json}"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	if _, err := harness.NewClient("http://"+addr).Do("POST", "/", nil, `"hello"`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := upstream.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if want := `{"body":"hello"}`; req.Body != want {
		t.Errorf("request body = %q, want %q", req.Body, want)
	}
}

func TestProxyJSONReplacesResponseBody(t *testing.T) {
	// The assignment replaces the response body too: a list comes
	// back to the client as its first element.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: `["a","b"]`})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    incoming-body: "body.json = body.json[0]"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := `"a"`; resp.Body != want {
		t.Errorf("response body = %q, want %q", resp.Body, want)
	}
}

func TestProxyJSONUnencodableAssignFails(t *testing.T) {
	// An assignment the encoder cannot write fails the flow:
	// the client sees a 500 naming the value, and the outlet
	// receives nothing - the bytes are produced before the
	// request leaves.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: `{}`})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-body: "body.json = flow"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("POST", "/", nil, `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 500 {
		t.Errorf("Status = %d, want 500", resp.Status)
	}
	if !strings.Contains(resp.Body, "cannot encode") {
		t.Errorf("Body = %q, want it to name the value", resp.Body)
	}
	if n := len(upstream.Requests()); n != 0 {
		t.Errorf("outlet received %d requests, want 0", n)
	}
}

func TestProxyJSONAssignNull(t *testing.T) {
	// Assigning None to body.json writes a null body on the way
	// out, and on the way back.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: `{}`})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-body: "body.json = None"
		|    incoming-body: "body.json = None"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	if _, err := harness.NewClient("http://"+addr).Do("POST", "/", nil, `{"old": true}`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req := upstream.LastRequest(); req == nil {
		t.Fatalf("outlet received no request")
	} else if req.Body != `null` {
		t.Errorf("request body = %q, want %q", req.Body, "null")
	}
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != `null` {
		t.Errorf("response body = %q, want %q", resp.Body, "null")
	}
}

func TestProxyIncomingBodyOverLimitFailsFlow(t *testing.T) {
	// A response body over the JSON limit fails the flow
	// too: 500 with the sizes, not a memory blowup.
	over := strings.Repeat("a", snippets.JSONLimit+2)
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: over})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    incoming-body: "flow.state[\"seen\"] = \"yes\""
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 500 {
		t.Errorf("Status = %d, want 500", resp.Status)
	}
	if !strings.Contains(resp.Body, "exceeds the "+strconv.Itoa(snippets.JSONLimit)+" byte limit") {
		t.Errorf("Body = %q, want it to name the limit", resp.Body)
	}
}

func TestProxyBodyFiltersShareState(t *testing.T) {
	// The whole round trip is one flow: the outgoing filter
	// records into the state, the incoming filter reads it.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: `{}`})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-body: "flow.state[\"sent\"] = \"yes\""
		|    incoming-body: "body.json[\"saw_outgoing\"] = flow.state[\"sent\"]"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("POST", "/", nil, `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := `{"saw_outgoing":"yes"}`; resp.Body != want {
		t.Errorf("response body = %q, want %q", resp.Body, want)
	}
}

func TestProxyFailInBodyFilterReachesClient(t *testing.T) {
	// A fail() inside a body filter answers the client with
	// its code and message, like the header filters do.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-body: "fail(\"bad request\", code=400)"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("POST", "/", nil, `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 400 || resp.Body != "bad request" {
		t.Errorf("response = %d %q, want 400 with the fail message", resp.Status, resp.Body)
	}
}

func TestProxyIncomingStreamFiltersEvents(t *testing.T) {
	// An event-stream response runs the incoming-event snippet once
	// per event, on the event's data - flow.state carries across
	// events, and each event reaches the client as it arrives.
	chunks := []string{
		"data: {\"n\":1}\n\n",
		"data: {\"n\":2}\n\n",
	}
	_, upURL := startUpstream(t, harness.Response{Status: 200, Headers: map[string]string{"Content-Type": "text/event-stream; charset=utf-8"}, Chunks: chunks})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    incoming-event:
		|        """
		|        event.json["count"] = flow.state.get("count", 0) + 1
		|        flow.state["count"] = event.json["count"]
		|        """
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "data: {\"n\":1,\"count\":1}\n\ndata: {\"n\":2,\"count\":2}\n\n"
	if resp.Status != 200 || resp.Body != want {
		t.Errorf("response = %d %q, want 200 with per-event counts", resp.Status, resp.Body)
	}
}

func TestProxyIncomingStreamPassesUntouched(t *testing.T) {
	// Events the snippet never touches cross verbatim, byte for
	// byte: comments, [DONE], and multi-line plain text whose join
	// is not JSON - event.json reads None, nothing is assigned.
	chunks := []string{
		": keep-alive\n\n",
		"data: hello\ndata: world\n\n",
		"data: [DONE]\n\n",
	}
	_, upURL := startUpstream(t, harness.Response{Status: 200, Headers: map[string]string{"Content-Type": "text/event-stream"}, Chunks: chunks})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    incoming-event: "if event.json != None:\n    event.json[\"x\"] = 1"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := strings.Join(chunks, "")
	if resp.Body != want {
		t.Errorf("response body = %q, want it verbatim: %q", resp.Body, want)
	}
}

func TestProxyIncomingStreamEventFields(t *testing.T) {
	// A touched event is rewritten with its fields: an assigned
	// name replaces the old event line, the untouched id stays, and
	// retry crosses as an int on the event, assignable back. A bare
	// event reads name as the spec's "message" default, and an
	// untouched one keeps its absent line.
	chunks := []string{
		"id: 7\nretry: 250\ndata: {\"n\":1}\n\n",
		"event: message\ndata: {\"n\":2}\n\n",
	}
	_, upURL := startUpstream(t, harness.Response{Status: 200, Headers: map[string]string{"Content-Type": "text/event-stream"}, Chunks: chunks})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    incoming-event:
		|        """
		|        if event.name != "message":
		|            fail("name not the message default")
		|        if event.json["n"] == 1:
		|            if event.retry != 250:
		|                fail("retry not decoded to int")
		|            event.name = "ping"
		|            event.retry = event.retry * 2
		|            event.json["x"] = 1
		|        """
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The first event is touched - the rewrite gains the event line
	// the snippet assigned - and the second stays verbatim: its name
	// read the default without writing it back.
	want := "event: ping\nid: 7\nretry: 500\ndata: {\"n\":1,\"x\":1}\n\n" +
		"event: message\ndata: {\"n\":2}\n\n"
	if resp.Body != want {
		t.Errorf("response body = %q, want %q", resp.Body, want)
	}
}

func TestProxyIncomingStreamUnfilteredCopies(t *testing.T) {
	// A route with no body filter never parses the stream: a pure
	// copy, event stream or not.
	chunks := []string{
		"data: {\"n\":1}\n\n",
		": comment\n\n",
	}
	_, upURL := startUpstream(t, harness.Response{Status: 200, Headers: map[string]string{"Content-Type": "text/event-stream"}, Chunks: chunks})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    incoming-headers: "headers[\"X-Seen\"] = \"yes\""
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != strings.Join(chunks, "") {
		t.Errorf("response body = %q, want the copy", resp.Body)
	}
}

func TestProxyIncomingStreamCutOnFailure(t *testing.T) {
	// The headers are out before the first event, so a snippet
	// failure mid-stream cuts the stream: the client keeps the
	// events that crossed, status and all.
	chunks := []string{
		"data: {\"n\":1}\n\n",
		"data: {\"n\":2}\n\n",
	}
	_, upURL := startUpstream(t, harness.Response{Status: 200, Headers: map[string]string{"Content-Type": "text/event-stream"}, Chunks: chunks})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    incoming-event: "if event.json[\"n\"] == 2:\n    fail(\"boom\")"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("Status = %d, want 200: the headers already left", resp.Status)
	}
	if want := "data: {\"n\":1}\n\n"; resp.Body != want {
		t.Errorf("response body = %q, want the first event only: %q", resp.Body, want)
	}
}

func TestProxyIncomingLanesSplitOnContentType(t *testing.T) {
	// The two lanes never mix: a JSON response takes the body lane
	// - the event snippet idle - and an event stream takes the
	// event lane, the body snippet idle. Each snippet fails if
	// its lane is not the one that ran.
	jsonUp, jsonURL := startUpstream(t, harness.Response{Status: 200, Body: `{"n":1}`})
	streamUp, streamURL := startUpstream(t, harness.Response{Status: 200, Headers: map[string]string{"Content-Type": "text/event-stream"}, Chunks: []string{"data: {\"n\":1}\n\n"}})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.jsonlane:
		|    address: "` + addr + `"
		|    url: "/json"
		|    outlet: "jsonup"
		|    incoming-event: "fail(\"event lane must not run\")"
		|    incoming-body: "body.json[\"x\"] = 1"
		|inlets.eventlane:
		|    address: "` + addr + `"
		|    url: "/stream"
		|    outlet: "streamup"
		|    incoming-event: "event.json[\"x\"] = 1"
		|    incoming-body: "fail(\"body lane must not run\")"
		|outlets.jsonup:
		|    address: "` + addrOf(jsonURL) + `"
		|    url: "` + jsonURL + `"
		|outlets.streamup:
		|    address: "` + addrOf(streamURL) + `"
		|    url: "` + streamURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/json", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 || resp.Body != `{"n":1,"x":1}` {
		t.Errorf("json lane response = %d %q, want the body filter's", resp.Status, resp.Body)
	}
	if n := len(jsonUp.Requests()); n != 1 {
		t.Errorf("json upstream received %d requests, want 1", n)
	}
	resp, err = harness.NewClient("http://"+addr).Do("GET", "/stream", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 || resp.Body != "data: {\"n\":1,\"x\":1}\n\n" {
		t.Errorf("stream lane response = %d %q, want the event filter's", resp.Status, resp.Body)
	}
	if n := len(streamUp.Requests()); n != 1 {
		t.Errorf("stream upstream received %d requests, want 1", n)
	}
}

func TestProxyIncomingBodyBuffersStream(t *testing.T) {
	// A body filter alone on an event-stream response buffers the
	// whole stream and filters it as one body: the joined stream is
	// not JSON, so it crosses byte for byte - the pre-stream
	// behavior, restored.
	chunks := []string{
		"data: {\"n\":1}\n\n",
		"data: [DONE]\n\n",
	}
	_, upURL := startUpstream(t, harness.Response{Status: 200, Headers: map[string]string{"Content-Type": "text/event-stream"}, Chunks: chunks})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    incoming-body: "if body.json != None:\n    body.json[\"x\"] = 1"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	_ = startProxy(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := strings.Join(chunks, ""); resp.Body != want {
		t.Errorf("response body = %q, want the buffered passthrough: %q", resp.Body, want)
	}
}

// buildProxy parses cnf against dir and builds a proxy on it,
// inheriting the given sockets. Serving is left to the caller.
func buildProxy(t *testing.T, cnf, dir string, listeners map[string]net.Listener) *proxy.Proxy {
	t.Helper()
	cfg, err := setup.Parse([]byte(cnf), dir)
	if err != nil {
		t.Fatalf("cannot parse setup: %v", err)
	}
	p, err := proxy.New(&proxy.ProxyOptions{Setup: cfg, Listeners: listeners})
	if err != nil {
		t.Fatalf("cannot build proxy: %v", err)
	}
	return p
}

func TestProxyReloadReusesTCPListener(t *testing.T) {
	// A reload hands the socket to the new generation: the new
	// outlet serves, with no rebind and no refused-dial window.
	_, up1URL := startUpstream(t, harness.Response{Status: 200, Body: "old"})
	_, up2URL := startUpstream(t, harness.Response{Status: 200, Body: "new"})
	addr := freePort(t)
	dir := t.TempDir()
	cnfFor := func(upURL string) string {
		return reindent(`
			|inlets.local:
			|    address: "` + addr + `"
			|    outlet: "up"
			|outlets.up:
			|    address: "` + addrOf(upURL) + `"
			|    url: "` + upURL + `"
			`)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p1 := buildProxy(t, cnfFor(up1URL), dir, nil)
	p1.Serve(ctx)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != "old" {
		t.Fatalf("body = %q, want the old outlet's", resp.Body)
	}

	p2 := buildProxy(t, cnfFor(up2URL), dir, p1.Masters())
	p1.Retire()
	p2.Serve(ctx)
	resp, err = harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error after reload: %v", err)
	}
	if resp.Body != "new" {
		t.Errorf("body = %q, want the new outlet's", resp.Body)
	}
}

func TestProxyReloadReusesUnixSocket(t *testing.T) {
	// The reused unix socket keeps its path: the retired copy
	// leaves it in place, and the new generation serves on it.
	_, up1URL := startUpstream(t, harness.Response{Status: 200, Body: "old"})
	_, up2URL := startUpstream(t, harness.Response{Status: 200, Body: "new"})
	sock := filepath.Join(t.TempDir(), "reuse.sock")
	dir := filepath.Dir(sock)
	cnfFor := func(upURL string) string {
		return reindent(`
			|inlets.local:
			|    address: "` + sock + `"
			|    outlet: "up"
			|outlets.up:
			|    address: "` + addrOf(upURL) + `"
			|    url: "` + upURL + `"
			`)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p1 := buildProxy(t, cnfFor(up1URL), dir, nil)
	p1.Serve(ctx)
	p2 := buildProxy(t, cnfFor(up2URL), dir, p1.Masters())
	p1.Retire()
	p2.Serve(ctx)
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket path after reload: %v", err)
	}
	resp, err := harness.NewClient("unix:"+sock).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error after reload: %v", err)
	}
	if resp.Body != "new" {
		t.Errorf("body = %q, want the new outlet's", resp.Body)
	}
}

func TestProxyReloadDropsListener(t *testing.T) {
	// A listener the new setup drops is closed and disposed: its
	// unix socket path is unlinked, the kept one still serves.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	dir := t.TempDir()
	kept := filepath.Join(dir, "kept.sock")
	dropped := filepath.Join(dir, "dropped.sock")
	cnfFor := func(inlets ...string) string {
		cnf := ""
		for _, inlet := range inlets {
			sock := filepath.Join(dir, inlet+".sock")
			cnf += reindent(`
				|inlets.` + inlet + `:
				|    address: "` + sock + `"
				|    outlet: "up"
				`)
		}
		return cnf + reindent(`
			|outlets.up:
			|    address: "`+addrOf(upURL)+`"
			|    url: "`+upURL+`"
			`)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p1 := buildProxy(t, cnfFor("kept", "dropped"), dir, nil)
	p1.Serve(ctx)
	p2 := buildProxy(t, cnfFor("kept"), dir, p1.Masters())
	p1.Retire()
	p2.Serve(ctx)
	if _, err := os.Stat(dropped); !os.IsNotExist(err) {
		t.Errorf("dropped socket path: %v, want it gone", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("kept socket path: %v", err)
	}
	resp, err := harness.NewClient("unix:"+kept).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error after reload: %v", err)
	}
	if resp.Body != "ok" {
		t.Errorf("body = %q, want the kept inlet's", resp.Body)
	}
}

func TestProxyReloadFailedBuildKeepsServing(t *testing.T) {
	// A failed build closes only what it bound: the inherited
	// sockets stay open for their owner, which keeps serving -
	// and a later reload still reuses them.
	_, up1URL := startUpstream(t, harness.Response{Status: 200, Body: "old"})
	_, up2URL := startUpstream(t, harness.Response{Status: 200, Body: "new"})
	addr := freePort(t)
	dir := t.TempDir()
	cnfFor := func(addr, upURL string) string {
		return reindent(`
			|inlets.local:
			|    address: "` + addr + `"
			|    outlet: "up"
			|outlets.up:
			|    address: "` + addrOf(upURL) + `"
			|    url: "` + upURL + `"
			`)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p1 := buildProxy(t, cnfFor(addr, up1URL), dir, nil)
	p1.Serve(ctx)

	// A port another process holds: the bind fails, and nothing
	// inherited may close for it.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot hold a port: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })
	cfg, err := setup.Parse([]byte(cnfFor(held.Addr().String(), up2URL)), dir)
	if err != nil {
		t.Fatalf("cannot parse setup: %v", err)
	}
	if _, err := proxy.New(&proxy.ProxyOptions{Setup: cfg, Listeners: p1.Masters()}); err == nil {
		t.Fatalf("error = nil, want the taken port to fail the build")
	}

	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error after failed reload: %v", err)
	}
	if resp.Body != "old" {
		t.Errorf("body = %q, want the old outlet still serving", resp.Body)
	}

	p2 := buildProxy(t, cnfFor(addr, up2URL), dir, p1.Masters())
	p1.Retire()
	p2.Serve(ctx)
	resp, err = harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error after reload: %v", err)
	}
	if resp.Body != "new" {
		t.Errorf("body = %q, want the new outlet's", resp.Body)
	}
}

func TestProxyReloadDrainsConnections(t *testing.T) {
	// A request already in flight when the reload lands rides the
	// retired generation to its end; the next one lands on the
	// new generation.
	_, up2URL := startUpstream(t, harness.Response{Status: 200, Body: "new"})
	seen := make(chan struct{}, 1)
	slow := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- struct{}{}
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(w, "old")
	})}
	slowLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot listen: %v", err)
	}
	go func() { _ = slow.Serve(slowLn) }()
	t.Cleanup(func() { _ = slow.Close() })
	up1URL := "http://" + slowLn.Addr().String()

	addr := freePort(t)
	dir := t.TempDir()
	cnfFor := func(upURL string) string {
		return reindent(`
			|inlets.local:
			|    address: "` + addr + `"
			|    outlet: "up"
			|outlets.up:
			|    address: "` + addrOf(upURL) + `"
			|    url: "` + upURL + `"
			`)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p1 := buildProxy(t, cnfFor(up1URL), dir, nil)
	p1.Serve(ctx)

	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
		if err != nil {
			done <- result{err: err}
			return
		}
		done <- result{body: resp.Body}
	}()
	<-seen

	p2 := buildProxy(t, cnfFor(up2URL), dir, p1.Masters())
	p1.Retire()
	p2.Serve(ctx)

	res := <-done
	if res.err != nil {
		t.Fatalf("in-flight request failed: %v", res.err)
	}
	if res.body != "old" {
		t.Errorf("in-flight body = %q, want the old generation to finish it", res.body)
	}
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error after reload: %v", err)
	}
	if resp.Body != "new" {
		t.Errorf("body = %q, want the new outlet's", resp.Body)
	}
}

func TestCopyListenerKeepsSocketAlive(t *testing.T) {
	// Closing the copy stops that generation's accept loop and
	// nothing else: the path stays, and the master still makes a
	// fresh copy. Closing the master is the disposal: the path
	// goes with it.
	path := filepath.Join(t.TempDir(), "copy.sock")
	master, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("cannot listen: %v", err)
	}

	first, err := proxy.CopyListener(master)
	if err != nil {
		t.Fatalf("cannot copy: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("cannot close copy: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket path after copy close: %v", err)
	}

	second, err := proxy.CopyListener(master)
	if err != nil {
		t.Fatalf("cannot copy again: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("cannot close copy: %v", err)
	}
	if err := master.Close(); err != nil {
		t.Fatalf("cannot close master: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("socket path after master close: %v, want it gone", err)
	}
}

func TestProxySnapshotsBuffered(t *testing.T) {
	// Buffered bodies snapshot byte-exact: the request through
	// the outgoing-body filter, the response through the
	// incoming-body one, the extensions from the content types,
	// and the log file holding the whole flow's log list - the
	// closing-flow logs included.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Headers: map[string]string{"Content-Type": "application/json"}, Body: `{"back":1}`})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-body:
		|        """
		|        outgoing.snapshot("both")
		|        body.json["sent"] = "yes"
		|        log({"kind": "out"})
		|        """
		|    incoming-body: "incoming.snapshot(\"both\")\nbody.json[\"came\"] = \"yes\""
		|    closing-flow: "log({\"kind\": \"close\"})"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	snaps := startProxySnapshots(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("POST", "/", map[string]string{"Content-Type": "application/json"}, `{"n":1}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != `{"back":1,"came":"yes"}` {
		t.Fatalf("response body = %q, want the filtered one", resp.Body)
	}
	// The request snapshot holds the filtered bytes the outlet
	// saw; the response snapshot the bytes the client saw.
	matches := snapshotFiles(t, snaps, "both")
	if len(matches) != 3 {
		t.Fatalf("files = %q, want the out, the in, and the log", matches)
	}
	for name, want := range map[string]string{
		"-out.json": `{"n":1,"sent":"yes"}`,
		"-in.json":  `{"back":1,"came":"yes"}`,
		"-log.json": `{"log":[{"kind":"out"},{"kind":"close"}]}` + "\n",
	} {
		data, ok := matches[name]
		if !ok {
			t.Errorf("no file ending in %q among %q", name, matches)
			continue
		}
		if data != want {
			t.Errorf("%s content = %q, want %q", name, data, want)
		}
	}
	// The request that reached the outlet matches its snapshot.
	if got := upstream.LastRequest().Body; got != `{"n":1,"sent":"yes"}` {
		t.Errorf("outlet body = %q, want the snapshot's", got)
	}
}

func TestProxySnapshotsStreamed(t *testing.T) {
	// Unbuffered bodies tee on the way through: the request
	// streams untouched, and so does the response - the snapshot
	// still holds the exact bytes.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Headers: map[string]string{"Content-Type": "text/plain"}, Body: "streamed-body"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    outgoing-headers: 'outgoing.snapshot("raw")'
		|    incoming-headers: "incoming.snapshot(\"raw\")"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	snaps := startProxySnapshots(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("POST", "/", map[string]string{"Content-Type": "text/plain"}, "streamed-req")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Body != "streamed-body" {
		t.Fatalf("response body = %q, want the untouched stream", resp.Body)
	}
	matches := snapshotFiles(t, snaps, "raw")
	for name, want := range map[string]string{
		"-out.txt": "streamed-req",
		"-in.txt":  "streamed-body",
	} {
		data, ok := matches[name]
		if !ok {
			t.Errorf("no file ending in %q among %q", name, matches)
			continue
		}
		if data != want {
			t.Errorf("%s content = %q, want %q", name, data, want)
		}
	}
	// The flow logged nothing: no log file, not an empty one.
	if _, ok := matches["-log.json"]; ok {
		t.Errorf("an empty log file crossed, want none")
	}
}

func TestProxySnapshotKeepsTmpOnFailure(t *testing.T) {
	// A flow that fails mid-stream keeps its .partial: the partial
	// payload is the artifact. The log file still commits.
	chunks := []string{"data: one\n\n"}
	_, upURL := startUpstream(t, harness.Response{Status: 200, Headers: map[string]string{"Content-Type": "text/event-stream"}, Chunks: chunks})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    incoming-headers: "incoming.snapshot(\"cut\")\nlog({\"seen\": \"yes\"})"
		|    incoming-event: "fail(\"boom\")"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	snaps := startProxySnapshots(t, cnf)
	resp, err := harness.NewClient("http://"+addr).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The stream cut before anything crossed: the snapshot file
	// stays .partial, the log file commits - a failed flow's logs
	// are the forensic pair with its partial payload.
	_ = resp
	entries, err := os.ReadDir(snaps)
	if err != nil {
		t.Fatalf("cannot read the snapshots dir: %v", err)
	}
	var partial, log int
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Name(), ".partial"):
			partial++
		case strings.HasSuffix(e.Name(), "-log.json"):
			log++
		}
	}
	if partial != 1 {
		t.Errorf(".partial files = %d, want the cut payload kept", partial)
	}
	if log != 1 {
		t.Errorf("log files = %d, want the committed log", log)
	}
}

// snapshotFiles reads every snapshot file whose name starts with
// the given prefix, keyed by the part from the kind marker on:
// -out.json, -in.txt, -log.json.
func snapshotFiles(t *testing.T, dir, prefix string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("cannot read the snapshots dir: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix+"-") {
			continue
		}
		var suffix string
		switch {
		case strings.HasSuffix(name, "-out.json"):
			suffix = "-out.json"
		case strings.HasSuffix(name, "-out.txt"):
			suffix = "-out.txt"
		case strings.HasSuffix(name, "-out.bin"):
			suffix = "-out.bin"
		case strings.HasSuffix(name, "-in.json"):
			suffix = "-in.json"
		case strings.HasSuffix(name, "-in.txt"):
			suffix = "-in.txt"
		case strings.HasSuffix(name, "-in.bin"):
			suffix = "-in.bin"
		case strings.HasSuffix(name, "-log.json"):
			suffix = "-log.json"
		default:
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("cannot read %s: %v", name, err)
		}
		out[suffix] = string(data)
	}
	return out
}

func TestProxyNotesFlush(t *testing.T) {
	// The closing-flow notes are global: two requests count
	// on the same map, and the flush lands it on the day file.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|    closing-flow: "notes[\"hits\"] = notes.get(\"hits\", 0) + 1"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	dir := t.TempDir()
	cfg, err := setup.Parse([]byte(cnf), dir)
	if err != nil {
		t.Fatalf("cannot parse setup: %v", err)
	}
	store, err := notes.New(dir, time.Now())
	if err != nil {
		t.Fatalf("cannot create notes store: %v", err)
	}
	t.Cleanup(store.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p, err := proxy.New(&proxy.ProxyOptions{Setup: cfg, Notes: store})
	if err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	p.Serve(ctx)
	client := harness.NewClient("http://" + addr)
	for range 2 {
		if _, err := client.Do("GET", "/", nil, ""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if upstream.LastRequest() == nil {
		t.Fatalf("outlet received no request")
	}
	if err := store.Flush(); err != nil {
		t.Fatalf("cannot flush notes: %v", err)
	}
	day := time.Now().Format("20060102") + ".json"
	data, err := os.ReadFile(filepath.Join(dir, day))
	if err != nil {
		t.Fatalf("cannot read day file: %v", err)
	}
	if string(data) != `{"hits":2}`+"\n" {
		t.Errorf("day file = %q, want the two requests' count", data)
	}
}

func TestProxyNotesHandover(t *testing.T) {
	// The reload hands the notes store over: the next generation
	// serves the same map, not a fresh read of the day files.
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "remote"
		|outlets.remote:
		|    address: "` + addr + `"
		|    url: "http://` + addr + `"
		`)
	dir := t.TempDir()
	cfg, err := setup.Parse([]byte(cnf), dir)
	if err != nil {
		t.Fatalf("cannot parse setup: %v", err)
	}
	store, err := notes.New(dir, time.Now())
	if err != nil {
		t.Fatalf("cannot create notes store: %v", err)
	}
	t.Cleanup(store.Close)
	p, err := proxy.New(&proxy.ProxyOptions{Setup: cfg, Notes: store})
	if err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	next, err := proxy.New(&proxy.ProxyOptions{
		Setup:     cfg,
		Notes:     store,
		Listeners: p.Masters(),
	})
	if err != nil {
		t.Fatalf("cannot start next generation: %v", err)
	}
	t.Cleanup(func() { _ = next.Stop() })
	if next.Notes() != store {
		t.Errorf("next generation notes = %p, want the handed over %p", next.Notes(), store)
	}
}

// startTLSProxy loads setup text with a TLS directory and
// starts a proxy on it.
func startTLSProxy(t *testing.T, cnf, tlsDir string) *proxy.Proxy {
	t.Helper()
	cfg, err := setup.Parse([]byte(cnf), t.TempDir())
	if err != nil {
		t.Fatalf("cannot parse setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p, err := proxy.New(&proxy.ProxyOptions{Setup: cfg, TLSPath: tlsDir})
	if err != nil {
		t.Fatalf("cannot start proxy: %v", err)
	}
	p.Serve(ctx)
	t.Cleanup(func() { _ = p.Stop() })
	return p
}

// tlsFiles reads one name's pair files off the TLS directory.
func tlsFiles(t *testing.T, tlsDir, name string) (certPEM, keyPEM []byte) {
	t.Helper()
	certPEM, err := os.ReadFile(filepath.Join(tlsDir, name+".cert"))
	if err != nil {
		t.Fatalf("cannot read cert: %v", err)
	}
	keyPEM, err = os.ReadFile(filepath.Join(tlsDir, name+".key"))
	if err != nil {
		t.Fatalf("cannot read key: %v", err)
	}
	return certPEM, keyPEM
}

func TestProxyTLSInlet(t *testing.T) {
	// A TLS inlet serves https off a certificate generated for
	// its own name - the address's, with none spelled in a url -
	// and the request reaches the outlet as usual.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "hi"})
	addr := freePort(t)
	tlsDir := t.TempDir()
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    tls-certs: ["+svc"]
		|    outlet: "up"
		|outlets.up:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	startTLSProxy(t, cnf, tlsDir)

	certPEM, _ := tlsFiles(t, tlsDir, "svc")
	client, err := harness.NewTLSClient(harness.TLSClientOptions{Addr: addr, Host: addr, RootPEM: certPEM})
	if err != nil {
		t.Fatalf("cannot create client: %v", err)
	}
	resp, err := client.Do("GET", "/x", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 || resp.Body != "hi" {
		t.Errorf("response = %d %q", resp.Status, resp.Body)
	}
	if req := upstream.LastRequest(); req == nil || req.Path != "/x" {
		t.Errorf("outlet saw %v", req)
	}
}

func TestProxyTLSInletPins(t *testing.T) {
	// A pinned inlet serves the clients holding one of the
	// pinned certificates, and answers the others - certless or
	// wrongly certified - with the client-certificate 404.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "hi"})
	addr := freePort(t)
	tlsDir := t.TempDir()
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    tls-certs: ["+svc"]
		|    tls-auth: ["+bar", "+baz"]
		|    outlet: "up"
		|outlets.up:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	log := captureLog(t)
	startTLSProxy(t, cnf, tlsDir)

	svcCert, _ := tlsFiles(t, tlsDir, "svc")
	drive := func(name string, present bool) harness.ResponseRecord {
		opts := harness.TLSClientOptions{Addr: addr, Host: addr, RootPEM: svcCert}
		if present {
			opts.CertPEM, opts.KeyPEM = tlsFiles(t, tlsDir, name)
		}
		client, err := harness.NewTLSClient(opts)
		if err != nil {
			t.Fatalf("cannot create client: %v", err)
		}
		resp, err := client.Do("GET", "/x", nil, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return resp
	}
	for _, name := range []string{"bar", "baz"} {
		if resp := drive(name, true); resp.Status != 200 {
			t.Errorf("pinned client %q: response = %d %q", name, resp.Status, resp.Body)
		}
	}
	// The server's own pair is a valid certificate that pins to
	// nothing: the wrong-cert shape.
	testCases := []struct {
		cert    string
		message string
	}{
		{"", "client certificate is missing"},
		{"svc", "client certificate is unknown"},
	}
	for _, tc := range testCases {
		resp := drive(tc.cert, tc.cert != "")
		if resp.Status != 404 || !strings.Contains(resp.Body, tc.message) {
			t.Errorf("client %q: response = %d %q", tc.cert, resp.Status, resp.Body)
		}
	}
	// The pinned clients' records name their certificate; the
	// 404s carry no tls-auth.
	got := logEntries(t, log)
	if !hasEntry(got, map[string]any{"method": "GET", "path": "/x", "code": float64(200), "tls-auth": "bar"}) {
		t.Errorf("records = %q, want one holding tls-auth bar", got)
	}
	if !hasEntry(got, map[string]any{"method": "GET", "path": "/x", "code": float64(200), "tls-auth": "baz"}) {
		t.Errorf("records = %q, want one holding tls-auth baz", got)
	}
	for _, m := range got {
		if m["code"] == float64(404) && m["tls-auth"] != nil {
			t.Errorf("404 record carries a tls-auth: %q", m)
		}
	}
}

func TestProxyTLSRefusals(t *testing.T) {
	// Inlets on one address must agree on TLS - all or none,
	// and on one cert list - and the url duplicate check stands
	// unchanged under pins.
	addr := freePort(t)
	tlsDir := t.TempDir()
	// The two inlets carry the tls lines each case spells; their
	// urls differ unless the case wants them equal.
	build := func(aTLS, bTLS, bURL string) string {
		return reindent(`
			|inlets.a:
			|    address: "` + addr + `"
			|    url: "/api"
			|    outlet: "up"
			` + aTLS + `
			|inlets.b:
			|    address: "` + addr + `"
			|    url: "` + bURL + `"
			|    outlet: "up"
			` + bTLS + `
			|outlets.up:
			|    address: "example.test:443"
			|    url: "https://example.test"
			`)
	}
	testCases := []struct{ name, cnf, error string }{
		{
			"certs differ",
			build("|    tls-certs: [\"+a\"]", "|    tls-certs: [\"+b\"]", "/two"),
			"tls-certs differ on shared address",
		},
		{
			"tls and plain mixed",
			build("|    tls-certs: [\"+a\"]", "", "/two"),
			"cannot mix tls and plain inlets",
		},
		{
			"duplicate url under different pins",
			build("|    tls-certs: [\"+a\"]\n|    tls-auth: [\"+bar\"]", "|    tls-certs: [\"+a\"]\n|    tls-auth: [\"+baz\"]", "/api"),
			"already served by",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := setup.Parse([]byte(tc.cnf), t.TempDir())
			if err != nil {
				t.Fatalf("cannot parse setup: %v", err)
			}
			_, err = proxy.New(&proxy.ProxyOptions{Setup: cfg, TLSPath: tlsDir})
			if err == nil || !strings.Contains(err.Error(), tc.error) {
				t.Errorf("error = %v, want %q", err, tc.error)
			}
		})
	}
}

func TestProxyOutletTLS(t *testing.T) {
	// The outlet dials TLS: it trusts the pinned anchor and
	// presents the auth identity, and the upstream records the
	// certificate it saw.
	tlsDir := t.TempDir()
	// The anchor is pre-created with the name the outlet's url
	// speaks, so the verification and the SNI both pass.
	store := rebeartls.NewStore(tlsDir)
	anchor, err := store.Pair("ca1", "upstream.example", true)
	if err != nil {
		t.Fatalf("cannot create anchor: %v", err)
	}
	upstream := harness.NewServer(harness.Response{Status: 200, Body: "hi"})
	upstreamCfg := &tls.Config{
		Certificates: []tls.Certificate{*anchor.Full},
		ClientAuth:   tls.RequireAnyClientCert,
	}
	upURL, err := upstream.StartTLS(upstreamCfg)
	if err != nil {
		t.Fatalf("cannot start upstream: %v", err)
	}
	t.Cleanup(func() { _ = upstream.Stop() })

	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    outlet: "up"
		|outlets.up:
		|    address: "` + strings.TrimPrefix(upURL, "https://") + `"
		|    url: "https://upstream.example"
		|    tls-certs: ["ca1"]
		|    tls-auth: ["+qux"]
		`)
	startTLSProxy(t, cnf, tlsDir)

	// The pair the proxy generated for the auth name, hashed the
	// way the upstream records it.
	qux, err := rebeartls.NewStore(tlsDir).Pair("qux", "qux", true)
	if err != nil {
		t.Fatalf("cannot read auth pair: %v", err)
	}
	sum := sha256.Sum256(qux.Leaf.Raw)

	resp, err := harness.NewClient("http://"+addr).Do("GET", "/x", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 || resp.Body != "hi" {
		t.Errorf("response = %d %q", resp.Status, resp.Body)
	}
	req := upstream.LastRequest()
	if req == nil {
		t.Fatalf("outlet received no request")
	}
	if req.PeerCert != hex.EncodeToString(sum[:]) {
		t.Errorf("peer cert = %q, want the qux pin", req.PeerCert)
	}
}

func TestProxyOutletTrustCerts(t *testing.T) {
	// The flag pins the leaf so the address stops mattering: the
	// same server cert, reached under a different host name, fails
	// normal verification and serves under pin mode.
	tlsDir := t.TempDir()
	store := rebeartls.NewStore(tlsDir)
	svc, err := store.Pair("svc", "other.example", true)
	if err != nil {
		t.Fatalf("cannot generate pair: %v", err)
	}
	upstream := harness.NewServer(harness.Response{Status: 200, Body: "hi"})
	upURL, err := upstream.StartTLS(&tls.Config{Certificates: []tls.Certificate{*svc.Full}})
	if err != nil {
		t.Fatalf("cannot start upstream: %v", err)
	}
	t.Cleanup(func() { _ = upstream.Stop() })

	addr := freePort(t)
	build := func(flags string) string {
		return reindent(`
			|inlets.local:
			|    address: "` + addr + `"
			|    outlet: "up"
			|outlets.up:
			|    address: "` + strings.TrimPrefix(upURL, "https://") + `"
			|    url: "https://api.example.test"
			|    tls-certs: ["svc"]
			` + flags)
	}
	for _, tc := range []struct {
		name   string
		flags  string
		status int
		body   string
	}{
		{"anchored", "", 502, "cannot reach outlet"},
		{"pinned", `|    flags: ["trust-certs"]` + "\n", 200, "hi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startTLSProxy(t, build(tc.flags), tlsDir)
			resp, err := harness.NewClient("http://"+addr).Do("GET", "/x", nil, "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.Status != tc.status || !strings.Contains(resp.Body, tc.body) {
				t.Errorf("response = %d %q, want %d %q", resp.Status, resp.Body, tc.status, tc.body)
			}
		})
	}
}

func TestProxyOutletUnsafeTLS(t *testing.T) {
	// The flag skips verification with the outlet carrying no
	// tls settings at all: the same server cert, reached under a
	// different host name, fails normal verification and
	// serves with the flag.
	tlsDir := t.TempDir()
	store := rebeartls.NewStore(tlsDir)
	svc, err := store.Pair("svc", "other.example", true)
	if err != nil {
		t.Fatalf("cannot generate pair: %v", err)
	}
	upstream := harness.NewServer(harness.Response{Status: 200, Body: "hi"})
	upURL, err := upstream.StartTLS(&tls.Config{Certificates: []tls.Certificate{*svc.Full}})
	if err != nil {
		t.Fatalf("cannot start upstream: %v", err)
	}
	t.Cleanup(func() { _ = upstream.Stop() })

	addr := freePort(t)
	build := func(flags string) string {
		return reindent(`
			|inlets.local:
			|    address: "` + addr + `"
			|    outlet: "up"
			|outlets.up:
			|    address: "` + strings.TrimPrefix(upURL, "https://") + `"
			|    url: "https://api.example.test"
			` + flags)
	}
	for _, tc := range []struct {
		name   string
		flags  string
		status int
		body   string
	}{
		{"verified", "", 502, "cannot reach outlet"},
		{"unsafe-tls", `|    flags: ["unsafe-tls"]` + "\n", 200, "hi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startTLSProxy(t, build(tc.flags), tlsDir)
			resp, err := harness.NewClient("http://"+addr).Do("GET", "/x", nil, "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.Status != tc.status || !strings.Contains(resp.Body, tc.body) {
				t.Errorf("response = %d %q, want %d %q", resp.Status, resp.Body, tc.status, tc.body)
			}
		})
	}
}

func TestProxyTLSHandshakeErrorLog(t *testing.T) {
	// A plain HTTP request to a TLS inlet dies before a request
	// record exists: net/http logs the handshake failure itself.
	// The server's ErrorLog routes it into the stream as an entry
	// carrying the error on its own field, the listener's address
	// and inlets on a log object - so stderr stays clean of raw
	// net/http log lines.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "hi"})
	addr := freePort(t)
	tlsDir := t.TempDir()
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    tls-certs: ["+svc"]
		|    outlet: "remote"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	log := captureLog(t)
	startTLSProxy(t, cnf, tlsDir)

	harness.NewClient("http://"+addr).Do("GET", "/x", nil, "")

	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, entry := range logEntries(t, log) {
			err, _ := entry["error"].(string)
			if !strings.Contains(err, "TLS handshake error") ||
				!strings.Contains(err, "client sent an HTTP request to an HTTPS server") {
				continue
			}
			want := []any{map[string]any{"inlets": []any{"local"}, "address": addr}}
			if !reflect.DeepEqual(entry["log"], []any(want)) {
				t.Errorf("log = %v, want inlets [local] and the listener address", entry["log"])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no handshake error entry in %s", log)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// distinctAddrs counts the distinct upstream connections the
// recorded requests arrived on: one address is one connection
// carrying them all, a keep-alive; a fresh address per request
// is connection churn.
func distinctAddrs(requests []harness.RequestRecord) int {
	set := map[string]bool{}
	for _, req := range requests {
		set[req.RemoteAddr] = true
	}
	return len(set)
}

func TestProxyOutletKeepAlive(t *testing.T) {
	// Serial requests ride one outlet connection: the upstream
	// sees one remote address for them all.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    url: "/api"
		|    outlet: "remote"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	startProxy(t, cnf)
	for i := 0; i < 5; i++ {
		resp, err := harness.NewClient("http://"+addr).Do("GET", "/api/x", nil, "")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if resp.Status != 200 {
			t.Fatalf("request %d: status %d", i, resp.Status)
		}
	}
	requests := upstream.Requests()
	if len(requests) != 5 {
		t.Fatalf("upstream saw %d requests, want 5", len(requests))
	}
	if n := distinctAddrs(requests); n != 1 {
		t.Errorf("distinct upstream connections = %d, want 1: keep-alive not reused", n)
	}
}

func TestProxyOutletKeepAliveConcurrent(t *testing.T) {
	// Concurrent requests share what the per-host idle pool
	// holds; with the default of 2 idle conns the sharing is
	// partial. A serial request after the burst still rides an
	// existing connection when one was pooled.
	upstream, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    url: "/api"
		|    outlet: "remote"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	startProxy(t, cnf)
	client := harness.NewClient("http://" + addr)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Do("GET", "/api/x", nil, "")
			if err != nil {
				t.Errorf("request: %v", err)
				return
			}
			if resp.Status != 200 {
				t.Errorf("status = %d, want 200", resp.Status)
			}
		}()
	}
	wg.Wait()
	requests := upstream.Requests()
	if len(requests) != 10 {
		t.Fatalf("upstream saw %d requests, want 10", len(requests))
	}
	// Fully concurrent requests cannot share an HTTP/1.1
	// connection; anything from 1 to 10 fresh dials is sound.
	// The pool's health shows once the burst settles: keep
	// issuing serial requests until one rides a connection
	// already seen - it must, since the idle pool has no limit.
	seen := map[string]bool{}
	for _, req := range requests {
		seen[req.RemoteAddr] = true
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Do("GET", "/api/x", nil, "")
		if err != nil {
			t.Fatalf("serial request after burst: %v", err)
		}
		if resp.Status != 200 {
			t.Fatalf("serial request: status %d", resp.Status)
		}
		requests = upstream.Requests()
		if last := requests[len(requests)-1]; seen[last.RemoteAddr] {
			return
		}
		seen[requests[len(requests)-1].RemoteAddr] = true
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no serial request rode a pooled connection in %s", 5*time.Second)
}

func TestProxyInletKeepAlive(t *testing.T) {
	// The inlet keeps the client connections alive: a client
	// driven by one transport dials once for serial requests.
	_, upURL := startUpstream(t, harness.Response{Status: 200, Body: "ok"})
	addr := freePort(t)
	cnf := reindent(`
		|inlets.local:
		|    address: "` + addr + `"
		|    url: "/api"
		|    outlet: "remote"
		|outlets.remote:
		|    address: "` + addrOf(upURL) + `"
		|    url: "` + upURL + `"
		`)
	startProxy(t, cnf)

	dials := 0
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dials++
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	base := "http://rebear.inlet"
	for i := 0; i < 3; i++ {
		resp, err := client.Get(base + "/api/x")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "ok" {
			t.Fatalf("request %d: %d %q", i, resp.StatusCode, body)
		}
	}
	if dials != 1 {
		t.Errorf("dials = %d, want 1: the inlet is not keeping the connection alive", dials)
	}
}
