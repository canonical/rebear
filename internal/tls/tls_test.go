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

package tls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/canonical/rebear/internal/setup"
)

// trustFlag and unsafeFlag are the config spellings the tests
// drive the outlet with; setup owns the names but exports no
// constants for them.
const (
	trustFlag  = "trust-certs"
	unsafeFlag = "unsafe-tls"
)

// startStore creates a Store over a fresh directory.
func startStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(t.TempDir())
}

func TestPairGenerates(t *testing.T) {
	// A missing pair generates both files, with the key private,
	// and the second read hands back the same pair untouched.
	store := startStore(t)
	pair, err := store.Pair("foo", "foo.example", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pair.Full == nil {
		t.Fatalf("generated pair has no key")
	}
	if pair.Leaf.Subject.CommonName != "foo.example" || pair.Leaf.DNSNames[0] != "foo.example" {
		t.Errorf("leaf names = %q %v, want foo.example", pair.Leaf.Subject.CommonName, pair.Leaf.DNSNames)
	}
	cert, err := os.Stat(filepath.Join(store.dir, "foo.cert"))
	if err != nil {
		t.Fatalf("cert missing: %v", err)
	}
	key, err := os.Stat(filepath.Join(store.dir, "foo.key"))
	if err != nil {
		t.Fatalf("key missing: %v", err)
	}
	if cert.Mode().Perm() != 0o644 || key.Mode().Perm() != 0o600 {
		t.Errorf("modes = %v %v, want 0644 0600", cert.Mode(), key.Mode())
	}
	again, err := store.Pair("foo", "other.example", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if again != pair || again.Leaf.Subject.CommonName != "foo.example" {
		t.Errorf("second read regenerated the pair")
	}
}

func TestPairMissingRefused(t *testing.T) {
	// A missing name generates only when asked to; otherwise
	// it is a missing certificate.
	store := startStore(t)
	_, err := store.Pair("foo", "foo.example", false)
	if err == nil || err.Error() != `tls "foo": missing certificate` {
		t.Errorf("error = %v, want missing certificate", err)
	}
	if _, err := os.Stat(filepath.Join(store.dir, "foo.cert")); !os.IsNotExist(err) {
		t.Errorf("nothing should have been written")
	}
}

func TestPairNeverOverwrites(t *testing.T) {
	// Existing files are loaded, never regenerated - a
	// regeneration would break every pinned peer.
	store := startStore(t)
	if _, err := store.Pair("foo", "foo.example", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	key, err := os.ReadFile(filepath.Join(store.dir, "foo.key"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	other := NewStore(store.dir)
	pair, err := other.Pair("foo", "other.example", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pair.Leaf.Subject.CommonName != "foo.example" {
		t.Errorf("CommonName = %q, want the existing foo.example", pair.Leaf.Subject.CommonName)
	}
	newKey, err := os.ReadFile(filepath.Join(store.dir, "foo.key"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(key) != string(newKey) {
		t.Errorf("key file was rewritten")
	}
}

func TestPairCertWithoutKey(t *testing.T) {
	// A cert alone loads as a leaf-only pair - the anchor and
	// pin uses never touch the key - and a key alone fails.
	store := startStore(t)
	pair, err := store.Pair("foo", "foo.example", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := os.Remove(filepath.Join(store.dir, "foo.key")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	other := NewStore(store.dir)
	leafOnly, err := other.Pair("foo", "foo.example", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if leafOnly.Full != nil {
		t.Errorf("pair carries a key without a key file")
	}
	if leafOnly.Leaf.Raw == nil || string(leafOnly.Leaf.Raw) != string(pair.Leaf.Raw) {
		t.Errorf("leaf-only pair lost the cert")
	}

	store = startStore(t)
	if _, err := store.Pair("foo", "foo.example", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := os.Remove(filepath.Join(store.dir, "foo.cert")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := NewStore(store.dir).Pair("foo", "foo.example", true); err == nil || err.Error() != `tls "foo": key without cert` {
		t.Errorf("error = %v, want key without cert", err)
	}
}

func TestServerConfigSNIPick(t *testing.T) {
	// The client hello's name picks inside the offered list;
	// an unknown or absent name gets the first entry.
	store := startStore(t)
	// Distinct leaves per name, pre-generated so the pick has
	// something to pick between.
	for _, name := range []string{"one", "two"} {
		if _, err := store.Pair(name, name+".example", true); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	cfg, err := store.ServerConfig([]*setup.Inlet{{
		Name:     "svc",
		TLSCerts: []string{"one", "two"},
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pick := func(name string) string {
		cert, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: name})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return leaf.Subject.CommonName
	}
	if got := pick("one.example"); got != "one.example" {
		t.Errorf("SNI one.example picked %q", got)
	}
	if got := pick("two.example"); got != "two.example" {
		t.Errorf("SNI two.example picked %q", got)
	}
	if got := pick("three.example"); got != "one.example" {
		t.Errorf("unknown SNI picked %q, want the first entry", got)
	}
	if got := pick(""); got != "one.example" {
		t.Errorf("no SNI picked %q, want the first entry", got)
	}
	if cfg.ClientAuth != tls.RequestClientCert {
		t.Errorf("ClientAuth = %v, want RequestClientCert", cfg.ClientAuth)
	}
}

func TestClientConfig(t *testing.T) {
	// The trust pool holds the named anchors, and the auth
	// names become the presented pairs.
	store := startStore(t)
	ca1, err := store.Pair("ca1", "ca1", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg, err := store.ClientConfig(&setup.Outlet{Name: "up", TLSCerts: []string{"ca1"}, TLSPlus: []string{"qux"}, TLSAuth: []string{"qux"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := ca1.Leaf.Verify(x509.VerifyOptions{Roots: cfg.RootCAs}); err != nil {
		t.Errorf("ca1 does not verify against the pool: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("presented %d pairs, want 1", len(cfg.Certificates))
	}
	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if leaf.Subject.CommonName != "qux" {
		t.Errorf("presented CommonName = %q, want qux", leaf.Subject.CommonName)
	}
}

func TestClientConfigDefaultCerts(t *testing.T) {
	// TLSDefaultCerts adds the system roots beside the named
	// anchors; without it the pool holds only those names, and
	// on its own it still dials TLS.
	store := startStore(t)
	if _, err := store.Pair("ca1", "ca1", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg, err := store.ClientConfig(&setup.Outlet{Name: "up", TLSCerts: []string{"ca1"}, TLSDefaultCerts: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RootCAs == nil {
		t.Fatalf("no pool built")
	}
	only, err := store.ClientConfig(&setup.Outlet{Name: "up", TLSCerts: []string{"ca1"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if only.RootCAs == nil {
		t.Fatalf("no pool built without default certs")
	}
	defaults, err := store.ClientConfig(&setup.Outlet{Name: "up", TLSDefaultCerts: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if defaults.RootCAs == nil {
		t.Fatalf("default certs alone built no pool")
	}
	if _, err := store.ClientConfig(&setup.Outlet{Name: "up"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCertSetInConnection(t *testing.T) {
	// A connection matches when its leaf is one of the
	// pinned ones, and the match names the pin it hit; an
	// unpinned leaf and a certless connection match nothing.
	store := startStore(t)
	bar, err := store.Pair("bar", "bar", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	baz, err := store.Pair("baz", "baz", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pins, err := store.NewCertSet([]string{"bar", "baz"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	state := func(leaf *x509.Certificate) *tls.ConnectionState {
		return &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	}
	if name, ok := pins.InConnection(state(bar.Leaf)); !ok || name != "bar" {
		t.Errorf("InConnection on the bar leaf = %q, %v; want bar, true", name, ok)
	}
	if name, ok := pins.InConnection(state(baz.Leaf)); !ok || name != "baz" {
		t.Errorf("InConnection on the baz leaf = %q, %v; want baz, true", name, ok)
	}
	other := startStore(t)
	qux, err := other.Pair("qux", "qux", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := pins.InConnection(state(qux.Leaf)); ok {
		t.Errorf("an unpinned leaf matched")
	}
	if _, ok := pins.InConnection(nil); ok {
		t.Errorf("a certless connection matched")
	}
	if _, ok := pins.InConnection(&tls.ConnectionState{}); ok {
		t.Errorf("an empty connection matched")
	}
	var none *CertSet
	if _, ok := none.InConnection(nil); ok {
		t.Errorf("nil pins named a pin")
	}
}

func TestGeneratePairIP(t *testing.T) {
	// An IP literal, with or without a port, becomes an IP SAN:
	// Go verifies IPs only against IPAddresses, so a DNS SAN
	// there would be a cert no client could verify.
	for _, hint := range []string{"10.0.0.5", "10.0.0.5:8443", "[::1]:8443", "::1"} {
		store := startStore(t)
		pair, err := store.Pair("ip", hint, true)
		if err != nil {
			t.Fatalf("hint %q: %v", hint, err)
		}
		host := strings.TrimPrefix(strings.TrimSuffix(hint, ":8443"), "[")
		host = strings.TrimSuffix(host, "]")
		if len(pair.Leaf.IPAddresses) != 1 || len(pair.Leaf.DNSNames) != 0 {
			t.Errorf("hint %q: SANs = %v %v, want one IP and no DNS", hint, pair.Leaf.IPAddresses, pair.Leaf.DNSNames)
		}
		if err := pair.Leaf.VerifyHostname(host); err != nil {
			t.Errorf("hint %q: cannot verify %q: %v", hint, host, err)
		}
	}
}

func TestGeneratePairDNS(t *testing.T) {
	// A name stays a DNS SAN - the common case must not move.
	store := startStore(t)
	pair, err := store.Pair("svc", "api.example.test:8443", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pair.Leaf.DNSNames) != 1 || pair.Leaf.DNSNames[0] != "api.example.test" || len(pair.Leaf.IPAddresses) != 0 {
		t.Errorf("SANs = %v %v, want the DNS name", pair.Leaf.DNSNames, pair.Leaf.IPAddresses)
	}
	if err := pair.Leaf.VerifyHostname("api.example.test"); err != nil {
		t.Errorf("cannot verify host: %v", err)
	}
}

func TestServerConfigGeneratesHost(t *testing.T) {
	// A generated leaf carries the endpoint's name, per inlet
	// Host: the url's host when it names one, else the tcp
	// address's, else the local name - an IP hint becoming an
	// IP SAN, a name a DNS one.
	testCases := []struct {
		name    string
		url     string
		kind    setup.Kind
		address string
		verify  string
	}{{
		name:    "url host wins",
		url:     "https://svc.example.test:8443",
		kind:    setup.KindTCP,
		address: "10.0.0.5:8443",
		verify:  "svc.example.test",
	}, {
		name:    "path-only url leaves the address naming",
		url:     "/api",
		kind:    setup.KindTCP,
		address: "10.0.0.5:8443",
		verify:  "10.0.0.5",
	}, {
		name:    "loopback address",
		kind:    setup.KindTCP,
		address: "127.0.0.1:8443",
		verify:  "127.0.0.1",
	}, {
		name:    "unix socket names localhost",
		kind:    setup.KindUnix,
		address: "/run/rebear.sock",
		verify:  "localhost",
	}, {
		name:    "unspecified address names localhost",
		kind:    setup.KindTCP,
		address: "0.0.0.0:443",
		verify:  "localhost",
	}}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			store := startStore(t)
			inlet := setup.Inlet{Name: "local", Kind: tc.kind, Address: tc.address, TLSCerts: []string{"svc"}, TLSPlus: []string{"svc"}}
			if tc.url != "" {
				u, err := url.Parse(tc.url)
				if err != nil {
					t.Fatalf("cannot parse url: %v", err)
				}
				inlet.URL = u
			}
			if _, err := store.ServerConfig([]*setup.Inlet{&inlet}); err != nil {
				t.Fatalf("cannot build the server config: %v", err)
			}
			pair, err := store.Pair("svc", "inlet", false)
			if err != nil {
				t.Fatalf("cannot read the pair: %v", err)
			}
			if pair.Leaf.Subject.CommonName != tc.verify {
				t.Errorf("CommonName = %q, want %q", pair.Leaf.Subject.CommonName, tc.verify)
			}
			if err := pair.Leaf.VerifyHostname(tc.verify); err != nil {
				t.Errorf("cannot verify %q: %v", tc.verify, err)
			}
		})
	}
}

func TestClientConfigTrustCerts(t *testing.T) {
	// The flag turns tls-certs into exact leaf pins: verification
	// skips the chain and the host name, and the pin is the whole
	// trust decision - an expired pinned leaf is still accepted.
	store := startStore(t)
	pinned, err := store.Pair("ca1", "ca1", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	other, err := store.Pair("ca2", "ca2", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := store.Pair("qux", "qux", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg, err := store.ClientConfig(&setup.Outlet{
		Name:     "up",
		TLSCerts: []string{"ca1"},
		TLSAuth:  []string{"qux"},
		Flags:    []string{trustFlag},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.InsecureSkipVerify || cfg.RootCAs != nil || cfg.VerifyConnection == nil {
		t.Errorf("cfg = %+v, want skip-verify with a pin hook and no pool", cfg)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("presented %d pairs, want the tls-auth one", len(cfg.Certificates))
	}
	if err := cfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{pinned.Leaf}}); err != nil {
		t.Errorf("pinned leaf refused: %v", err)
	}
	if err := cfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{other.Leaf}}); err == nil {
		t.Errorf("unpinned leaf accepted")
	}
	if err := cfg.VerifyConnection(tls.ConnectionState{}); err == nil {
		t.Errorf("certless connection accepted")
	}

	// Default certs add no pin. Load refuses them with this
	// flag, but a directly built Outlet may carry them: the
	// named cert is still pinned.
	defaultCfg, err := store.ClientConfig(&setup.Outlet{
		Name:            "up",
		TLSCerts:        []string{"ca1"},
		TLSDefaultCerts: true,
		Flags:           []string{trustFlag},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := defaultCfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{pinned.Leaf}}); err != nil {
		t.Errorf("pinned leaf refused beside the default certs: %v", err)
	}
	if err := defaultCfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{other.Leaf}}); err == nil {
		t.Errorf("unpinned leaf accepted beside the default certs")
	}
	for _, outlet := range []*setup.Outlet{{
		Name:  "up",
		Flags: []string{trustFlag},
	}, {
		Name:            "up",
		TLSDefaultCerts: true,
		Flags:           []string{trustFlag},
	}} {
		if _, err := store.ClientConfig(outlet); err == nil || !strings.Contains(err.Error(), "needs tls-certs") {
			t.Errorf("outlet %+v: error = %v, want needs tls-certs", outlet, err)
		}
	}

	// An expired leaf of exactly the pinned DER still passes: the
	// pin decides, not the chain's validity dates.
	expired := expiredLeaf(t)
	if err := os.WriteFile(filepath.Join(store.dir, "old.cert"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: expired.Raw}), 0o644); err != nil {
		t.Fatalf("cannot write expired cert: %v", err)
	}
	oldCfg, err := store.ClientConfig(&setup.Outlet{
		Name:     "up",
		TLSCerts: []string{"old"},
		Flags:    []string{trustFlag},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := oldCfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{expired}}); err != nil {
		t.Errorf("expired pinned leaf refused: %v", err)
	}
}

func TestClientConfigUnsafeTLS(t *testing.T) {
	// The flag skips verification with no pin hook: the named
	// pairs still load, the system pool is welcome, and the
	// tls-auth pair is still presented.
	store := startStore(t)
	if _, err := store.Pair("ca1", "ca1", true); err != nil {
		t.Fatalf("cannot generate pair: %v", err)
	}
	if _, err := store.Pair("qux", "qux", true); err != nil {
		t.Fatalf("cannot generate pair: %v", err)
	}
	named, err := store.ClientConfig(&setup.Outlet{
		Name:     "up",
		TLSCerts: []string{"ca1"},
		TLSAuth:  []string{"qux"},
		Flags:    []string{unsafeFlag},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !named.InsecureSkipVerify || named.RootCAs == nil || named.VerifyConnection != nil {
		t.Errorf("cfg = %+v, want skip-verify, the named pool loaded, and no pin hook", named)
	}
	if len(named.Certificates) != 1 {
		t.Errorf("presented %d pairs, want the tls-auth one", len(named.Certificates))
	}
	bare, err := store.ClientConfig(&setup.Outlet{Name: "up", Flags: []string{unsafeFlag}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bare.InsecureSkipVerify || bare.RootCAs != nil || bare.VerifyConnection != nil {
		t.Errorf("bare cfg = %+v, want skip-verify alone", bare)
	}
	defaults, err := store.ClientConfig(&setup.Outlet{Name: "up", TLSDefaultCerts: true, Flags: []string{unsafeFlag}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !defaults.InsecureSkipVerify || defaults.RootCAs == nil {
		t.Errorf("defaults cfg = %+v, want the system pool loaded beside the skip", defaults)
	}
}

// expiredLeaf builds a self-signed leaf that expired yesterday.
func expiredLeaf(t *testing.T) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("cannot generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "old"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cannot create cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("cannot parse cert: %v", err)
	}
	return leaf
}
