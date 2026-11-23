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

package setup

import (
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// sampleOutlet is a minimal outlet section the tests point their
// inlets at.
const sampleOutlet = `
outlets.sample:
    address: "example.test:80"
    url: "http://example.test"
`

// TestParseInletWithoutOutletField pins the routing rule: no
// outlet is selected by name match, so the field stays empty
// even when an outlet bears the inlet's own name.
func TestParseInletWithoutOutletField(t *testing.T) {
	cnf := reindent(`
		|inlets.local:
		|    address: "127.0.0.1:8080"
		|outlets.local:
		|    address: "example.test:80"
		|    url: "http://example.test"
		`)
	cfg, err := Parse([]byte(cnf), "/cfg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Inlets[0].Outlet != "" {
		t.Errorf("Outlet = %q, want empty", cfg.Inlets[0].Outlet)
	}
}
func TestParseTCPInletAndOutlet(t *testing.T) {
	// A tcp inlet over an address and an outlet over an address
	// and url, with the snippet settings held verbatim.
	cnf := reindent(`
		|inlets.local:
		|    address: "127.0.0.1"
		|    outlet: "upstream"
		|    opening-flow:
		|        """
		|        count = 0
		|        """
		|    outgoing-headers:
		|        """
		|        headers["X-Test"] = "yes"
		|        """
		|    outgoing-body:
		|        """
		|        body.json["n"] = 1
		|        """
		|outlets.upstream:
		|    address: "api.example.com:443"
		|    url: "https://api.example.com"
		|    incoming-headers:
		|        """
		|        headers.pop("Server", None)
		|        """
		`)
	cfg, err := Parse([]byte(cnf), "/cfg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Inlets) != 1 || len(cfg.Outlets) != 1 {
		t.Fatalf("got %d inlets, %d outlets, want 1 and 1", len(cfg.Inlets), len(cfg.Outlets))
	}
	inlet := cfg.Inlets[0]
	if inlet.Name != "local" || inlet.Kind != KindTCP {
		t.Errorf("inlet = %q (%s), want local (tcp)", inlet.Name, inlet.Kind)
	}
	if inlet.Address != "127.0.0.1:80" || inlet.URL != nil {
		t.Errorf("address/url = %q / %v, want the port-normalized address and no url", inlet.Address, inlet.URL)
	}
	if inlet.OpeningFlow != "count = 0\n" {
		t.Errorf("OpeningFlow = %q", inlet.OpeningFlow)
	}
	if inlet.OutgoingHeaders != "headers[\"X-Test\"] = \"yes\"\n" {
		t.Errorf("OutgoingHeaders = %q", inlet.OutgoingHeaders)
	}
	if inlet.OutgoingBody == nil || inlet.OutgoingBody.Body != "body.json[\"n\"] = 1\n" {
		t.Errorf("OutgoingBody = %+v", inlet.OutgoingBody)
	}
	outlet := cfg.Outlets[0]
	if outlet.Name != "upstream" || outlet.Address != "api.example.com:443" {
		t.Errorf("outlet = %q %q", outlet.Name, outlet.Address)
	}
	if outlet.URL.Scheme != "https" || outlet.URL.Host != "api.example.com" || outlet.URL.Path != "/" {
		t.Errorf("URL = %v, want https://api.example.com/", outlet.URL)
	}
	if outlet.IncomingHeaders != "headers.pop(\"Server\", None)\n" {
		t.Errorf("IncomingHeaders = %q", outlet.IncomingHeaders)
	}
}
func TestParseUnixInlet(t *testing.T) {
	// A unix inlet separates the socket path from the url, and a
	// relative path is resolved against the config directory. A
	// path-only url stores nothing beyond the path.
	cnf := reindent(`
		|inlets.sock:
		|    address: "./foo/bar"
		|    outlet: "sample"
		|    url: "/v2"
		`) + sampleOutlet
	cfg, err := Parse([]byte(cnf), "/cfg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	inlet := cfg.Inlets[0]
	if inlet.Kind != KindUnix {
		t.Errorf("Kind = %s, want unix", inlet.Kind)
	}
	if inlet.Address != "/cfg/foo/bar" {
		t.Errorf("Address = %q, want /cfg/foo/bar", inlet.Address)
	}
	if u := inlet.URL; u == nil || u.Path != "/v2" || u.Host != "" || u.Scheme != "" {
		t.Errorf("URL = %v, want the bare path /v2", inlet.URL)
	}
}
func TestParseUnixInletDefaultURL(t *testing.T) {
	// Without a url, the inlet holds no routing constraint: any
	// host, any path.
	cnf := reindent(`
		|inlets.sock:
		|    address: "/abs/path/sock"
		|    outlet: "sample"
		`) + sampleOutlet
	cfg, err := Parse([]byte(cnf), "/cfg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	inlet := cfg.Inlets[0]
	if inlet.URL != nil {
		t.Errorf("URL = %v, want nil", inlet.URL)
	}
	// An absolute socket path is kept as is.
	if inlet.Address != "/abs/path/sock" {
		t.Errorf("Address = %q", inlet.Address)
	}
}
func TestParseUnixOutlet(t *testing.T) {
	// A unix outlet separates the socket path from the URL, and a
	// relative path is resolved against the config directory.
	cnf := reindent(`
		|inlets.sock:
		|    address: "/abs/in.sock"
		|    outlet: "up"
		|outlets.up:
		|    address: "./up/sock"
		|    url: "http://internal.example/api"
		`)
	cfg, err := Parse([]byte(cnf), "/cfg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	outlet := cfg.Outlets[0]
	if outlet.Kind != KindUnix {
		t.Errorf("Kind = %s, want unix", outlet.Kind)
	}
	if outlet.Address != "/cfg/up/sock" {
		t.Errorf("Address = %q, want /cfg/up/sock", outlet.Address)
	}
	if u := outlet.URL; u == nil || u.Scheme != "http" || u.Host != "internal.example" || u.Path != "/api" {
		t.Errorf("URL = %v, want http://internal.example/api", outlet.URL)
	}
}

func TestParseUnixOutletDerivedURL(t *testing.T) {
	// Without a url, a unix outlet keeps an empty URL: the request
	// then keeps the address the client named.
	cnf := reindent(`
		|inlets.sock:
		|    address: "/abs/in.sock"
		|    outlet: "up"
		|outlets.up:
		|    address: "/abs/up.sock"
		`)
	cfg, err := Parse([]byte(cnf), "/cfg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Outlets[0].URL != nil {
		t.Errorf("URL = %v, want nil", cfg.Outlets[0].URL)
	}
}

func TestParseEscapedURLPath(t *testing.T) {
	// Escapes in a url decode at parse, both forms of the setting:
	// the mount and the outlet prefix work on the decoded path,
	// %2F included.
	cnf := reindent(`
		|inlets.sock:
		|    address: "/abs/in.sock"
		|    outlet: "remote"
		|    url: "/a%2Fb/c"
		|outlets.remote:
		|    address: "x:80"
		|    url: "http://x/p%2Fq"
		`)
	cfg, err := Parse([]byte(cnf), "/cfg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u := cfg.Inlets[0].URL; u == nil || u.Path != "/a/b/c" {
		t.Errorf("inlet URL = %v, want the decoded path /a/b/c", cfg.Inlets[0].URL)
	}
	if u := cfg.Outlets[0].URL; u == nil || u.Path != "/p/q" {
		t.Errorf("outlet URL = %v, want the decoded path /p/q", cfg.Outlets[0].URL)
	}
}

func TestParseAddressFromURL(t *testing.T) {
	// An omitted address takes the url's host: the endpoint is
	// the same as spelling the address with it. An explicit
	// address always wins.
	testCases := []struct {
		name    string
		cnf     string
		kind    Kind
		address string
	}{{
		name: "inlet with port",
		cnf: `
			|inlets.local:
			|    url: "http://api.example.com:8080/v1"
			`,
		kind:    KindTCP,
		address: "api.example.com:8080",
	}, {
		name: "inlet portless",
		cnf: `
			|inlets.local:
			|    url: "http://api.example.com"
			`,
		kind:    KindTCP,
		address: "api.example.com:80",
	}, {
		name: "outlet",
		cnf: `
			|outlets.up:
			|    url: "https://api.example.com/api"
			`,
		kind:    KindTCP,
		address: "api.example.com:443",
	}, {
		name: "explicit address wins",
		cnf: `
			|outlets.up:
			|    address: "other.example:9"
			|    url: "https://api.example.com/api"
			`,
		kind:    KindTCP,
		address: "other.example:9",
	}}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(reindent(tc.cnf)), "/cfg")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var kind Kind
			var address string
			if strings.HasPrefix(tc.cnf, "\n\t\t\t|inlets") {
				kind, address = cfg.Inlets[0].Kind, cfg.Inlets[0].Address
			} else {
				kind, address = cfg.Outlets[0].Kind, cfg.Outlets[0].Address
			}
			if kind != tc.kind || address != tc.address {
				t.Errorf("address = %s %q, want %s %q", kind, address, tc.kind, tc.address)
			}
		})
	}
}

// A tcp outlet takes no url at all: the address is enough, the
// missing port arriving with the scheme the outlet speaks.
func TestParseOutletAddressOnly(t *testing.T) {
	testCases := []struct {
		name    string
		cnf     string
		address string
		scheme  string
	}{{
		name: "remote tcp gets 443 https",
		cnf: `
			|outlets.up:
			|    address: "api.example.com"
			`,
		address: "api.example.com:443",
		scheme:  "https",
	}, {
		name: "remote tcp keeps its port",
		cnf: `
			|outlets.up:
			|    address: "api.example.com:9"
			`,
		address: "api.example.com:9",
		scheme:  "https",
	}, {
		name: "localhost gets 80 http",
		cnf: `
			|outlets.up:
			|    address: "localhost"
			`,
		address: "localhost:80",
		scheme:  "http",
	}, {
		name: "localhost keeps a non-443 port",
		cnf: `
			|outlets.up:
			|    address: "localhost:8080"
			`,
		address: "localhost:8080",
		scheme:  "http",
	}, {
		name: "localhost on 443 says https",
		cnf: `
			|outlets.up:
			|    address: "localhost:443"
			`,
		address: "localhost:443",
		scheme:  "https",
	}, {
		name: "loopback IP gets 80 http",
		cnf: `
			|outlets.up:
			|    address: "127.0.0.1"
			`,
		address: "127.0.0.1:80",
		scheme:  "http",
	}, {
		name: "loopback IPv6 gets 80 http",
		cnf: `
			|outlets.up:
			|    address: "[::1]"
			`,
		address: "[::1]:80",
		scheme:  "http",
	}, {
		name: "a unix outlet stays http",
		cnf: `
			|outlets.up:
			|    address: "/sock"
			`,
		address: "/sock",
		scheme:  "http",
	}, {
		name: "tls settings flip a local target to https",
		cnf: `
			|outlets.up:
			|    address: "localhost"
			|    tls-certs: ["ca1"]
			`,
		address: "localhost:443",
		scheme:  "https",
	}, {
		name: "tls settings flip a unix outlet too",
		cnf: `
			|outlets.up:
			|    address: "/sock"
			|    tls-certs: ["ca1"]
			`,
		address: "/sock",
		scheme:  "https",
	}, {
		name: "an explicit http url wins the scheme",
		cnf: `
			|outlets.up:
			|    address: "api.example.com"
			|    url: "http://api.example.com"
			`,
		address: "api.example.com:80",
		scheme:  "http",
	}}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(reindent(tc.cnf)), "/cfg")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			outlet := cfg.Outlets[0]
			if outlet.Address != tc.address {
				t.Errorf("Address = %q, want %q", outlet.Address, tc.address)
			}
			if got := outlet.Scheme(); got != tc.scheme {
				t.Errorf("Scheme() = %q, want %q", got, tc.scheme)
			}
		})
	}
}

func TestParseEndpointHost(t *testing.T) {
	// The endpoint's name follows the settings: the url's host
	// when it names one, else the tcp address's, else localhost.
	testCases := []struct {
		name string
		cnf  string
		want string
	}{{
		name: "inlet url host wins",
		cnf: `
			|inlets.local:
			|    address: "127.0.0.1:8443"
			|    url: "https://api.example.test/"
			|    tls-certs: ["svc"]
			`,
		want: "api.example.test",
	}, {
		name: "inlet url host keeps its port",
		cnf: `
			|inlets.local:
			|    url: "https://api.example.test:8443/"
			|    tls-certs: ["svc"]
			`,
		want: "api.example.test:8443",
	}, {
		name: "inlet path-only url leaves the address naming",
		cnf: `
			|inlets.local:
			|    address: "127.0.0.1:8080"
			|    url: "/v1"
			`,
		want: "127.0.0.1:8080",
	}, {
		name: "inlet tls address drops the default port",
		cnf: `
			|inlets.local:
			|    address: "127.0.0.1"
			|    tls-certs: ["svc"]
			`,
		want: "127.0.0.1",
	}, {
		name: "inlet plain address drops the default port",
		cnf: `
			|inlets.local:
			|    address: "srv.example.test"
			`,
		want: "srv.example.test",
	}, {
		name: "inlet unspecified address names localhost",
		cnf: `
			|inlets.local:
			|    address: "0.0.0.0"
			`,
		want: "localhost",
	}, {
		name: "inlet socket names localhost",
		cnf: `
			|inlets.local:
			|    address: "/sock"
			`,
		want: "localhost",
	}, {
		name: "outlet url host wins",
		cnf: `
			|outlets.up:
			|    address: "10.0.0.5:443"
			|    url: "https://api.example.test/api"
			`,
		want: "api.example.test",
	}, {
		name: "outlet address drops the default port",
		cnf: `
			|outlets.up:
			|    address: "api.example.com"
			`,
		want: "api.example.com",
	}, {
		name: "outlet loopback keeps a non-default port",
		cnf: `
			|outlets.up:
			|    address: "127.0.0.1:8080"
			`,
		want: "127.0.0.1:8080",
	}, {
		name: "outlet unspecified address names localhost",
		cnf: `
			|outlets.up:
			|    address: "0.0.0.0:443"
			`,
		want: "localhost",
	}, {
		name: "outlet socket names localhost",
		cnf: `
			|outlets.up:
			|    address: "/sock"
			|    tls-certs: ["ca1"]
			`,
		want: "localhost",
	}}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(reindent(tc.cnf)), "/cfg")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var host string
			if strings.HasPrefix(tc.cnf, "\n\t\t\t|inlets") {
				host = cfg.Inlets[0].Host()
			} else {
				host = cfg.Outlets[0].Host()
			}
			if host != tc.want {
				t.Errorf("Host() = %q, want %q", host, tc.want)
			}
		})
	}
}

func TestParseMissingAddress(t *testing.T) {
	// No address and nothing to take one from - no url, or a
	// url without a host - is the same complaint either way.
	testCases := []struct {
		name string
		cnf  string
	}{{
		name: "inlet with no url",
		cnf: `
			|inlets.local:
			|    opening-flow: "pass"
			`,
	}, {
		name: "inlet with a path-only url",
		cnf: `
			|inlets.local:
			|    url: "/v1"
			`,
	}, {
		name: "outlet with no url",
		cnf: `
			|outlets.up:
			|    opening-flow: "pass"
			`,
	}, {
		name: "outlet with a path-only url",
		cnf: `
			|outlets.up:
			|    url: "/api"
			`,
	}}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(reindent(tc.cnf)), "/cfg")
			if err == nil || !strings.Contains(err.Error(), "missing address") {
				t.Fatalf("error = %v, want missing address", err)
			}
		})
	}
}

func TestParseStreamJSONKeys(t *testing.T) {
	// The nested-key Body filter form turns into key bindings, each
	// with its own snippet.
	cnf := reindent(`
		|inlets.local:
		|    address: "127.0.0.1:8080"
		|    outlet: "sample"
		|    outgoing-body:
		|        choices:
		|            """
		|            save(value)
		|            """
		|        messages.role:
		|            """
		|            value
		|            """
		`)
	cfg, err := Parse([]byte(cnf+sampleOutlet), "/cfg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f := cfg.Inlets[0].OutgoingBody
	if f == nil || f.Body != "" || len(f.Keys) != 2 {
		t.Fatalf("filter = %+v, want two key bindings", f)
	}
	// Each binding carries one path segment per nesting level, so a
	// wire key containing a dot can never be confused with nesting.
	want := map[string]string{
		"choices":       "save(value)\n",
		"messages.role": "value\n",
	}
	for _, b := range f.Keys {
		key := strings.Join(b.Path, ".")
		if want[key] != b.Snippet {
			t.Errorf("binding %q = %q, want %q", key, b.Snippet, want[key])
		}
		delete(want, key)
	}
	if len(want) > 0 {
		t.Errorf("missing bindings: %q", want)
	}
	if !slices.Equal(f.Keys[1].Path, []string{"messages", "role"}) {
		t.Errorf("Path = %q, want [messages role]", f.Keys[1].Path)
	}
}
func TestParseMidlets(t *testing.T) {
	// Midlet sections parse like the other kinds: the routing
	// fields and the snippet settings, with no address or url.
	cnf := reindent(`
		|inlets.local:
		|    address: "127.0.0.1:8080"
		|    midlets: ["strip"]
		|midlets.strip:
		|    midlets: ["redact"]
		|    outlet: "sample"
		|    opening-flow:
		|        """
		|        count = 0
		|        """
		|    outgoing-headers:
		|        """
		|        headers["X-Test"] = "yes"
		|        """
		|    outgoing-body:
		|        """
		|        body.json["n"] = 1
		|        """
		|midlets.redact:
		|    opening-flow:
		|        """
		|        pass
		|        """
		`) + sampleOutlet
	cfg, err := Parse([]byte(cnf), "/cfg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Midlets) != 2 {
		t.Fatalf("got %d midlets, want 2", len(cfg.Midlets))
	}
	if got := cfg.Inlets[0].Midlets; !slices.Equal(got, []string{"strip"}) {
		t.Errorf("inlet Midlets = %q, want [strip]", got)
	}
	var midlet *Midlet
	for _, m := range cfg.Midlets {
		if m.Name == "strip" {
			midlet = m
		}
	}
	if midlet == nil {
		t.Fatalf("no strip midlet in %v", cfg.Midlets)
	}
	if midlet.Name != "strip" || !slices.Equal(midlet.Midlets, []string{"redact"}) || midlet.Outlet != "sample" {
		t.Errorf("midlet = %q %q %q", midlet.Name, midlet.Midlets, midlet.Outlet)
	}
	if midlet.OpeningFlow != "count = 0\n" {
		t.Errorf("OpeningFlow = %q", midlet.OpeningFlow)
	}
	if midlet.OutgoingHeaders != "headers[\"X-Test\"] = \"yes\"\n" {
		t.Errorf("OutgoingHeaders = %q", midlet.OutgoingHeaders)
	}
	if midlet.OutgoingBody == nil || midlet.OutgoingBody.Body != "body.json[\"n\"] = 1\n" {
		t.Errorf("OutgoingBody = %+v", midlet.OutgoingBody)
	}
}

func TestParseMidletCarriesOutlet(t *testing.T) {
	// An inlet with no outlet field and a midlet carrying one:
	// the midlet's field settles the route, the inlet's stays
	// empty.
	cnf := reindent(`
		|inlets.local:
		|    address: "127.0.0.1:8080"
		|    midlets: ["strip"]
		|midlets.strip:
		|    outlet: "sample"
		`) + sampleOutlet
	cfg, err := Parse([]byte(cnf), "/cfg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Inlets[0].Outlet != "" {
		t.Errorf("Outlet = %q, want empty", cfg.Inlets[0].Outlet)
	}
}

func TestParseMidletCycleAllowed(t *testing.T) {
	// A midlet pair naming each other parses: the chain runs
	// each midlet once and skips the second listing, so no
	// loop is refused at load.
	cnf := reindent(`
		|inlets.local:
		|    address: "127.0.0.1:8080"
		|    midlets: ["a"]
		|midlets.a:
		|    midlets: ["b"]
		|midlets.b:
		|    midlets: ["a"]
		`) + sampleOutlet
	if _, err := Parse([]byte(cnf), "/cfg"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// reindent strips each line up to and including its first "|",
// keeping the suffix as the line, and drops lines without one. It
// lets a cnf sample sit pipe-prefixed and indented inside a table
// entry while the content keeps cnf's column-consistent form.
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

func TestParseErrors(t *testing.T) {
	testCases := []struct {
		name string
		cnf  string
		want string
	}{{
		name: "inlet without address",
		cnf: `
			|inlets.bad:
			|    url: "/x"
			`,
		want: "missing address",
	}, {
		name: "outlet without address",
		cnf: `
			|outlets.bad:
			|    opening-flow:
			|        """
			|        """
			`,
		want: "missing address",
	}, {
		name: "address holding a url",
		cnf: `
			|inlets.bad:
			|    address: "https://127.0.0.1:8080"
			`,
		want: `inlets.bad.address: "https://127.0.0.1:8080" looks like a URL, use ` + "`url`" + ` field instead`,
	}, {
		name: "address with a mid-slash path",
		cnf: `
			|inlets.bad:
			|    address: "sock/api"
			`,
		want: `unix socket path must start with "/" or "./"`,
	}, {
		name: "inlet https url",
		cnf: `
			|inlets.bad:
			|    address: "127.0.0.1:8080"
			|    url: "https://localhost/v1"
			`,
		want: "must be http",
	}, {
		name: "outlet ftp url",
		cnf: `
			|outlets.bad:
			|    address: "x:80"
			|    url: "ftp://x"
			`,
		want: "must be http or https",
	}, {
		name: "url without a path or scheme",
		cnf: `
			|outlets.bad:
			|    address: "/sock"
			|    url: "internal.example/api"
			`,
		want: "must be a /path or a URL",
	}, {
		name: "url with a query",
		cnf: `
			|outlets.bad:
			|    address: "/sock"
			|    url: "/api?v=1"
			`,
		want: "must not hold a query or a fragment",
	}, {
		name: "unknown inlet field",
		cnf: `
			|inlets.bad:
			|    address: "127.0.0.1:8080"
			|    wrong: 1
			`,
		want: "unknown field"}, {
		name: "inlet with unknown midlet",
		cnf: `
			|inlets.bad:
			|    address: "127.0.0.1:8080"
			|    midlets: ["nope"]
			`,
		want: `cannot find "nope" midlet`,
	}, {
		name: "midlet with unknown next midlet",
		cnf: `
			|midlets.bad:
			|    midlets: ["nope"]
			`,
		want: `cannot find "nope" midlet`,
	}, {
		name: "midlet with unknown outlet",
		cnf: `
			|midlets.bad:
			|    outlet: "nope"
			`,
		want: `cannot find "nope" outlet`,
	}, {
		name: "unknown outlet field",
		cnf: `
			|outlets.bad:
			|    address: "x:80"
			|    url: "http://x"
			|    wrong: 1
			`,
		want: "unknown field",
	}, {
		name: "unknown top field",
		cnf: `
			|wrong: 1
			`,
		want: "unknown field",
	}}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(reindent(tc.cnf)), "/cfg")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParseDecodeErrorNamesSection(t *testing.T) {
	// The per-section decode names the section an error sat in,
	// and no json: prefix leaks: the user wrote cnf, not json.
	testCases := []struct {
		name string
		cnf  string
		want string
	}{{
		name: "unknown inlet field",
		cnf: `
			|inlets.local:
			|    address: "127.0.0.1:8080"
			|    listen: "/sock"
			`,
		want: `inlets.local: unknown field "listen"`,
	}, {
		name: "unknown midlet field",
		cnf: `
			|midlets.strip:
			|    address: "127.0.0.1:8080"
			`,
		want: `midlets.strip: unknown field "address"`,
	}, {
		name: "unknown outlet field",
		cnf: `
			|outlets.up:
			|    address: "x:80"
			|    url: "http://x"
			|    connect: "y"
			`,
		want: `outlets.up: unknown field "connect"`,
	}, {
		name: "type mismatch",
		cnf: `
			|inlets.local:
			|    address: "127.0.0.1:8080"
			|    opening-flow: 3
			`,
		want: `inlets.local: cannot unmarshal number into Go struct field rawInlet.opening-flow of type string`,
	}, {
		name: "bad body filter form",
		cnf: `
			|inlets.local:
			|    address: "127.0.0.1:8080"
			|    outgoing-body: 3
			`,
		want: `inlets.local: not a snippet or a set of key bindings`,
	}, {
		name: "unknown top field",
		cnf: `
			|wrong: 1
			`,
		want: `unknown field "wrong"`,
	}}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(reindent(tc.cnf)), "/cfg")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want exactly %q", err, tc.want)
			}
		})
	}
}

func TestParseMalformedCNF(t *testing.T) {
	// A file cnf itself rejects fails loading with that error.
	_, err := Parse([]byte("inlet:\n\taddress: \"unterminated"), "/cfg")
	if err == nil {
		t.Fatalf("error = nil, want a cnf error")
	}
}

func TestLoadRoundTrip(t *testing.T) {
	// Load reads the file and anchors relative paths at the file's
	// own directory.
	dir := t.TempDir()
	path := filepath.Join(dir, "rebear.cnf")
	content := "inlets.s:\n    address: \"./sock\"\n    outlet: \"up\"\noutlets.up:\n    address: \"x:80\"\n    url: \"http://x\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("cannot write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Inlets[0].Address != filepath.Join(dir, "sock") {
		t.Errorf("Address = %q, want it inside %q", cfg.Inlets[0].Address, dir)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.cnf"))
	if err == nil || !os.IsNotExist(err) {
		t.Fatalf("error = %v, want not exist", err)
	}
}

func TestParseAddressPortNormalized(t *testing.T) {
	// A tcp address carries its port after parse, so the socket
	// is named one way everywhere: an inlet takes 80, the http
	// default; an outlet takes the url's port, 80 on an http url
	// and 443 otherwise - a path-only url among them. Unix paths
	// and explicit ports cross untouched, and the url itself is
	// never rewritten.
	testCases := []struct {
		name    string
		cnf     string
		address string
		urlHost string
	}{{
		name: "inlet portless",
		cnf: `
			|inlets.local:
			|    address: "example.test"
			|    url: "http://api.example.com/v1"
			`,
		address: "example.test:80",
		urlHost: "api.example.com",
	}, {
		name: "inlet ipv6 portless",
		cnf: `
			|inlets.local:
			|    address: "[::1]"
			|    url: "http://api.example.com/v1"
			`,
		address: "[::1]:80",
		urlHost: "api.example.com",
	}, {
		name: "inlet explicit port",
		cnf: `
			|inlets.local:
			|    address: "example.test:8080"
			|    url: "http://api.example.com/v1"
			`,
		address: "example.test:8080",
		urlHost: "api.example.com",
	}, {
		name: "outlet http url",
		cnf: `
			|outlets.up:
			|    address: "example.test"
			|    url: "http://example.test/api"
			`,
		address: "example.test:80",
		urlHost: "example.test",
	}, {
		name: "outlet https url",
		cnf: `
			|outlets.up:
			|    address: "example.test"
			|    url: "https://example.test/api"
			`,
		address: "example.test:443",
		urlHost: "example.test",
	}, {
		name: "outlet path-only url",
		cnf: `
			|outlets.up:
			|    address: "example.test"
			|    url: "/api"
			`,
		address: "example.test:443",
		urlHost: "",
	}, {
		name: "outlet unix",
		cnf: `
			|outlets.up:
			|    address: "/run/up.sock"
			`,
		address: "/run/up.sock",
		urlHost: "",
	}}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(reindent(tc.cnf)), "/cfg")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var address string
			var u *url.URL
			if strings.HasPrefix(tc.cnf, "\n\t\t\t|inlets") {
				address, u = cfg.Inlets[0].Address, cfg.Inlets[0].URL
			} else {
				address, u = cfg.Outlets[0].Address, cfg.Outlets[0].URL
			}
			if address != tc.address {
				t.Errorf("address = %q, want %q", address, tc.address)
			}
			if u == nil {
				if tc.urlHost != "" {
					t.Errorf("url = nil, want the host %q", tc.urlHost)
				}
				return
			}
			if u.Host != tc.urlHost {
				t.Errorf("url host = %q, want %q", u.Host, tc.urlHost)
			}
		})
	}
}

func TestParseInletTLS(t *testing.T) {
	// The tls settings parse and store; with them the implied
	// scheme is https, an https url is accepted, and the default
	// port is 443.
	cnf := reindent(`
		|inlets.local:
		|    address: "127.0.0.1"
		|    tls-certs: ["svc"]
		|    tls-auth: ["bar", "baz"]
		|outlets.up:
		|    address: "example.test:443"
		|    url: "https://example.test"
		`)
	cfg, err := Parse([]byte(cnf), "/cfg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	inlet := cfg.Inlets[0]
	if !slices.Equal(inlet.TLSCerts, []string{"svc"}) || !slices.Equal(inlet.TLSAuth, []string{"bar", "baz"}) {
		t.Errorf("tls settings = %v %v", inlet.TLSCerts, inlet.TLSAuth)
	}
	if inlet.Address != "127.0.0.1:443" {
		t.Errorf("address = %q, want 127.0.0.1:443", inlet.Address)
	}

	cnf = reindent(`
		|inlets.local:
		|    url: "https://api.example.test/v1"
		|    tls-certs: ["svc"]
		|outlets.up:
		|    address: "example.test:443"
		|    url: "https://example.test"
		`)
	cfg, err = Parse([]byte(cnf), "/cfg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inlet := cfg.Inlets[0]; inlet.Address != "api.example.test:443" {
		t.Errorf("address = %q, want api.example.test:443", inlet.Address)
	}
}

func TestParseInletTLSErrors(t *testing.T) {
	// An http url conflicts with tls, an https url needs it,
	// client pins need server certs, and a tls name is a
	// filename, never a path.
	testCases := []struct{ name, cnf, error string }{{
		name:  "http url with tls",
		cnf:   "inlets.local:\n    address: '127.0.0.1:8080'\n    url: 'http://api.example.test'\n    tls-certs: ['svc']",
		error: "must be https",
	}, {
		name:  "https url without tls",
		cnf:   "inlets.local:\n    address: '127.0.0.1:8080'\n    url: 'https://api.example.test'",
		error: "must be http",
	}, {
		name:  "tls-auth without tls-certs",
		cnf:   "inlets.local:\n    address: '127.0.0.1:8080'\n    tls-auth: ['bar']",
		error: "tls-auth needs tls-certs",
	}, {
		name:  "path in a name",
		cnf:   "inlets.local:\n    address: '127.0.0.1:8080'\n    tls-certs: ['a/b']",
		error: "must match",
	}, {
		name:  "empty name",
		cnf:   "inlets.local:\n    address: '127.0.0.1:8080'\n    tls-certs: ['']",
		error: "must match",
	}, {
		name:  "uppercase name",
		cnf:   "inlets.local:\n    address: '127.0.0.1:8080'\n    tls-certs: ['Bad']",
		error: "must match",
	}, {
		name:  "star on an inlet",
		cnf:   "inlets.local:\n    address: '127.0.0.1:8080'\n    tls-certs: ['*']",
		error: "must match",
	}}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cnf := tc.cnf + "\noutlets.up:\n    address: \"example.test:443\"\n    url: \"https://example.test\"\n"
			_, err := Parse([]byte(cnf), "/cfg")
			if err == nil || !strings.Contains(err.Error(), tc.error) {
				t.Errorf("error = %v, want %q", err, tc.error)
			}
		})
	}
}

func TestParseOutletTLS(t *testing.T) {
	// An outlet carries the tls settings too, and an http url
	// conflicts with them. The "*" of tls-certs is split into
	// TLSDefaultCerts, leaving real names in TLSCerts.
	cnf := reindent(`
		|inlets.local:
		|    address: "127.0.0.1:8080"
		|outlets.up:
		|    address: "example.test:443"
		|    url: "https://example.test"
		|    tls-certs: ["ca1", "*"]
		|    tls-auth: ["qux"]
		`)
	cfg, err := Parse([]byte(cnf), "/cfg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	outlet := cfg.Outlets[0]
	if !slices.Equal(outlet.TLSCerts, []string{"ca1"}) || !slices.Equal(outlet.TLSAuth, []string{"qux"}) {
		t.Errorf("tls settings = %v %v", outlet.TLSCerts, outlet.TLSAuth)
	}
	if !outlet.TLSDefaultCerts {
		t.Errorf("TLSDefaultCerts = false, want the * pulled out of tls-certs")
	}

	cnf = reindent(`
		|inlets.local:
		|    address: "127.0.0.1:8080"
		|outlets.up:
		|    address: "example.test:80"
		|    url: "http://example.test"
		|    tls-certs: ["ca1"]
		`)
	_, err = Parse([]byte(cnf), "/cfg")
	if err == nil || !strings.Contains(err.Error(), "must be https") {
		t.Errorf("error = %v, want must be https", err)
	}
}

func TestParseOutletTLSUnixURL(t *testing.T) {
	// A unix outlet under TLS speaks https where the url leaves
	// the scheme open - no url or a path-only one - while an
	// explicit http url stays a conflict.
	testCases := []struct {
		name  string
		url   string
		error string
	}{
		{"no url", "", ""},
		{"path only", "/api", ""},
		{"https url", "https://internal.example/api", ""},
		{"http url", "http://up.example/api", "must be https"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cnf := "inlets.local:\n    address: \"/run/rebear.sock\"\noutlets.up:\n    address: \"/run/up.sock\"\n"
			if tc.url != "" {
				cnf += "    url: \"" + tc.url + "\"\n"
			}
			cnf += "    tls-certs: [\"ca1\"]\n"
			_, err := Parse([]byte(cnf), "/cfg")
			if tc.error == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.error) {
				t.Errorf("error = %v, want %q", err, tc.error)
			}
		})
	}
}

func TestParseTLSPlus(t *testing.T) {
	// The "+" prefix marks the names that may generate: it is
	// trimmed off the stored lists and recorded in TLSPlus. The
	// star is never a generated name - it becomes
	// TLSDefaultCerts instead.
	cnf := reindent(`
		|inlets.local:
		|    address: "127.0.0.1:8080"
		|    tls-certs: ["+svc", "old"]
		|    tls-auth: ["+bar"]
		|outlets.up:
		|    address: "example.test:443"
		|    url: "https://example.test"
		|    tls-certs: ["+ca1", "*"]
		|    tls-auth: ["qux"]
		`)
	cfg, err := Parse([]byte(cnf), "/cfg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	inlet := cfg.Inlets[0]
	if !slices.Equal(inlet.TLSCerts, []string{"svc", "old"}) || !slices.Equal(inlet.TLSAuth, []string{"bar"}) {
		t.Errorf("inlet lists = %v %v, want the + trimmed", inlet.TLSCerts, inlet.TLSAuth)
	}
	if !slices.Equal(inlet.TLSPlus, []string{"svc", "bar"}) {
		t.Errorf("inlet TLSPlus = %v, want [svc bar]", inlet.TLSPlus)
	}
	outlet := cfg.Outlets[0]
	if !slices.Equal(outlet.TLSCerts, []string{"ca1"}) || !slices.Equal(outlet.TLSAuth, []string{"qux"}) {
		t.Errorf("outlet lists = %v %v, want the + trimmed and the * out", outlet.TLSCerts, outlet.TLSAuth)
	}
	if !outlet.TLSDefaultCerts {
		t.Errorf("outlet TLSDefaultCerts = false, want the * recorded")
	}
	if !slices.Equal(outlet.TLSPlus, []string{"ca1"}) {
		t.Errorf("outlet TLSPlus = %v, want [ca1]", outlet.TLSPlus)
	}
}

func TestParseDefaultCerts(t *testing.T) {
	// The "*" of an outlet's tls-certs is split into
	// TLSDefaultCerts, leaving only real names in TLSCerts.
	// Inlets never accept it, and the trust flag refuses it.
	build := func(certs, flags string) string {
		return reindent(`
			|inlets.local:
			|    address: "127.0.0.1:8080"
			|    outlet: "up"
			|outlets.up:
			|    address: "example.test:443"
			|    url: "https://example.test"
			|    tls-certs: [` + certs + `]
			` + flags)
	}
	testCases := []struct {
		name     string
		cnf      string
		certs    []string
		defaults bool
		wantErr  string
	}{{
		"no star", build(`"ca1"`, ""), []string{"ca1"}, false, "",
	}, {
		"star only", build(`"*"`, ""), nil, true, "",
	}, {
		"star and name", build(`"ca1", "*"`, ""), []string{"ca1"}, true, "",
	}, {
		"plus star", build(`"+ca1", "+*"`, ""), []string{"ca1"}, true, "",
	}, {
		"star with the flag", build(`"ca1", "*"`, `|    flags: ["trust-certs"]`+"\n"), nil, true, "pinned names and no default certs",
	}, {
		"star with unsafe-tls", build(`"ca1", "*"`, `|    flags: ["unsafe-tls"]`+"\n"), []string{"ca1"}, true, "",
	}, {
		"star on an inlet", reindent(`
			|inlets.local:
			|    address: "127.0.0.1:8080"
			|    tls-certs: ["*"]
			|outlets.up:
			|    address: "example.test:443"
			|    url: "https://example.test"
			`), nil, false, "must match",
	}}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.cnf), "/cfg")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			outlet := cfg.Outlets[0]
			if !slices.Equal(outlet.TLSCerts, tc.certs) {
				t.Errorf("TLSCerts = %v, want %v", outlet.TLSCerts, tc.certs)
			}
			if outlet.TLSDefaultCerts != tc.defaults {
				t.Errorf("TLSDefaultCerts = %v, want %v", outlet.TLSDefaultCerts, tc.defaults)
			}
		})
	}
}

func TestParseFlags(t *testing.T) {
	// Each flag's rules live in one case of checkFlags: the
	// name, duplicates, the sniplet kind, and its context.
	with := func(inlet, outlet string) string {
		return reindent(`
			|inlets.local:
			|    address: "127.0.0.1:8080"
			|    outlet: "up"
			` + inlet + `|outlets.up:
			|    address: "example.test:443"
			|    url: "https://example.test"
			` + outlet)
	}
	certs := `|    tls-certs: ["ca1"]` + "\n"
	flag := `|    flags: ["trust-certs"]` + "\n"
	unsafe := `|    flags: ["unsafe-tls"]` + "\n"
	if cfg, err := Parse([]byte(with("", certs+flag)), "/cfg"); err != nil {
		t.Fatalf("valid flag: %v", err)
	} else if !cfg.Outlets[0].TrustCertsFlag() {
		t.Errorf("TrustCertsFlag = false, want the flag recognized")
	}
	// unsafe-tls alone is legal with no tls settings, and still
	// forces the https side of the scheme rule.
	if cfg, err := Parse([]byte(with("", unsafe)), "/cfg"); err != nil {
		t.Fatalf("valid unsafe-tls: %v", err)
	} else if !cfg.Outlets[0].UnsafeTLSFlag() || !cfg.Outlets[0].HasTLS() {
		t.Errorf("UnsafeTLSFlag/HasTLS = %t/%t, want the flag recognized as TLS", cfg.Outlets[0].UnsafeTLSFlag(), cfg.Outlets[0].HasTLS())
	}
	testCases := []struct{ name, cnf, error string }{{
		"unknown flag", with("", certs+`|    flags: ["trust-tls-cert"]`+"\n"), "unknown flag",
	}, {
		"bad flag name", with("", certs+`|    flags: ["Bad/Name"]`+"\n"), "must match",
	}, {
		"duplicate flag", with("", certs+`|    flags: ["trust-certs", "trust-certs"]`+"\n"), "duplicate flag",
	}, {
		"duplicate unsafe-tls", with("", `|    flags: ["unsafe-tls", "unsafe-tls"]`+"\n"), "duplicate flag",
	}, {
		"unsafe-tls on an inlet", with(unsafe, ""), "only valid on outlets",
	}, {
		"flags combined", with("", certs+`|    flags: ["trust-certs", "unsafe-tls"]`+"\n"), "cannot be used together",
	}, {
		"unsafe-tls refuses an http url", reindent(`
			|inlets.local:
			|    address: "127.0.0.1:8080"
			|    outlet: "up"
			|outlets.up:
			|    address: "example.test:443"
			|    url: "http://example.test"
			|    flags: ["unsafe-tls"]
			`), "must be https",
	}, {
		"on an inlet", with(flag, certs), "only valid on outlets",
	}, {
		"on a midlet", reindent(`
			|inlets.local:
			|    address: "127.0.0.1:8080"
			|    midlets: ["redact"]
			|    outlet: "up"
			|midlets.redact:
			|    flags: ["trust-certs"]
			|outlets.up:
			|    address: "example.test:443"
			|    url: "https://example.test"
			|    tls-certs: ["ca1"]
			`), "only valid on outlets",
	}, {
		"no tls", with("", flag), "pinned names and no default certs",
	}, {
		"only tls-auth", with("", `|    tls-auth: ["qux"]`+"\n"+flag), "pinned names and no default certs",
	}, {
		"default certs", with("", `|    tls-certs: ["ca1", "*"]`+"\n"+flag), "pinned names and no default certs",
	}}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.cnf), "/cfg")
			if err == nil || !strings.Contains(err.Error(), tc.error) {
				t.Errorf("error = %v, want %q", err, tc.error)
			}
		})
	}
}
