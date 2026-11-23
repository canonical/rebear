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

// Package tls loads and generates the certificate pairs the
// tls-certs and tls-auth settings name, and builds the TLS
// configurations the proxy serves and dials with. It is the only
// package that touches crypto/tls and crypto/x509.
package tls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/canonical/rebear/internal/setup"
)

// Pair is one named certificate: its leaf always present, and
// the full Go certificate - key included - only when a key
// file sits beside the cert, as trust anchors and pins never
// need one.
type Pair struct {
	Leaf *x509.Certificate
	Full *tls.Certificate
}

// pin is the pair's exact identity: the leaf's raw DER hashed.
func (p *Pair) pin() [sha256.Size]byte {
	return sha256.Sum256(p.Leaf.Raw)
}

// Store reads and generates the named pairs under one TLS
// directory, memoizing each name so a setup sharing a name
// shares the read.
type Store struct {
	dir   string
	pairs map[string]*Pair
}

// NewStore creates a Store serving the given directory.
func NewStore(dir string) *Store {
	return &Store{dir: dir, pairs: map[string]*Pair{}}
}

// Pair reads the named pair, generating a self-signed one when
// the cert file is missing and generate says the config asked
// for it - a missing name that may not generate is an error.
// The cn is the name a generated leaf carries - the name the
// routing knows - and only the first generation of a name sees
// it. Existing files are never overwritten: a regeneration
// would silently break every peer that pinned the old cert.
func (s *Store) Pair(name, cn string, generate bool) (*Pair, error) {
	if pair, ok := s.pairs[name]; ok {
		return pair, nil
	}
	pair, err := loadPair(s.dir, name)
	if err != nil {
		return nil, err
	}
	if pair == nil {
		if !generate {
			return nil, fmt.Errorf("tls %q: missing certificate", name)
		}
		pair, err = generatePair(s.dir, name, cn)
		if err != nil {
			return nil, err
		}
	}
	s.pairs[name] = pair
	return pair, nil
}

// Generate writes a self-signed pair for the name, refusing to
// touch an existing one - the same rule Pair generates under,
// spelled for the one-shot CLI use.
func (s *Store) Generate(name, cn string) (*Pair, error) {
	pair, err := loadPair(s.dir, name)
	if err != nil {
		return nil, err
	}
	if pair != nil {
		return nil, fmt.Errorf("tls %q: certificate already exists", name)
	}
	pair, err = generatePair(s.dir, name, cn)
	if err != nil {
		return nil, err
	}
	s.pairs[name] = pair
	return pair, nil
}

// loadPair reads the pair from disk, or returns nil when the
// cert file is missing. A key beside the cert joins the pair; a
// key without its cert is an inconsistent directory and fails.
func loadPair(dir, name string) (*Pair, error) {
	certPEM, certErr := os.ReadFile(filepath.Join(dir, name+".cert"))
	keyPEM, keyErr := os.ReadFile(filepath.Join(dir, name+".key"))
	if certErr != nil {
		if os.IsNotExist(certErr) && keyErr != nil && os.IsNotExist(keyErr) {
			return nil, nil
		}
		if os.IsNotExist(certErr) {
			return nil, fmt.Errorf("tls %q: key without cert", name)
		}
		return nil, fmt.Errorf("tls %q: cannot read cert: %v", name, certErr)
	}
	if keyErr != nil && !os.IsNotExist(keyErr) {
		return nil, fmt.Errorf("tls %q: cannot read key: %v", name, keyErr)
	}
	if keyErr == nil {
		certificate, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("tls %q: %v", name, err)
		}
		return pairOf(name, certificate)
	}
	// A cert alone: the anchor and pin uses, which never touch
	// the key.
	leaf, err := readLeaf(name, certPEM)
	if err != nil {
		return nil, err
	}
	return &Pair{Leaf: leaf}, nil
}

// pairOf splits a loaded key pair into its leaf and its full
// Go certificate.
func pairOf(name string, certificate tls.Certificate) (*Pair, error) {
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("tls %q: cannot parse cert: %v", name, err)
	}
	certificate.Leaf = leaf
	return &Pair{Leaf: leaf, Full: &certificate}, nil
}

// readLeaf parses the first certificate off a PEM block.
func readLeaf(name string, certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("tls %q: cert is not a PEM certificate", name)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("tls %q: cannot parse cert: %v", name, err)
	}
	return leaf, nil
}

// generatePair creates a self-signed pair and writes it: ECDSA
// P-256, the cn as subject and SAN, key 0600, cert 0644, both
// files fresh - an existing one is never touched. An IP literal
// becomes an IP SAN: Go only verifies IPs against IPAddresses,
// never against a DNS name or the CN.
func generatePair(dir, name, cn string) (*Pair, error) {
	if host, _, err := net.SplitHostPort(cn); err == nil {
		cn = host
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("tls %q: cannot generate key: %v", name, err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("tls %q: cannot generate serial: %v", name, err)
	}
	// CA bits on a leaf are harmless and let a generated cert
	// act as its own trust anchor.
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	if ip := net.ParseIP(cn); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{cn}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("tls %q: cannot generate cert: %v", name, err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("tls %q: cannot marshal key: %v", name, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("tls %q: cannot create directory: %v", name, err)
	}
	if err := writeNew(filepath.Join(dir, name+".cert"), 0o644, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return nil, fmt.Errorf("tls %q: cannot write cert: %v", name, err)
	}
	if err := writeNew(filepath.Join(dir, name+".key"), 0o600, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})); err != nil {
		return nil, fmt.Errorf("tls %q: cannot write key: %v", name, err)
	}
	return pairOf(name, tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	})
}

// writeNew writes a file that must not exist, so a generation
// never overwrites anything.
func writeNew(path string, mode os.FileMode, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// ServerConfig builds the shared listener's configuration from
// the inlets of one address: their agreed tls-certs list
// offered with the client's SNI picking inside it, and client
// certificates requested but never verified - the pins do that.
// The returned config is nil when no inlet carries TLS.
func (s *Store) ServerConfig(inlets []*setup.Inlet) (*tls.Config, error) {
	var pairs []*Pair
	for _, inlet := range inlets {
		for _, name := range inlet.TLSCerts {
			pair, err := s.Pair(name, inlet.Host(), slices.Contains(inlet.TLSPlus, name))
			if err != nil {
				return nil, fmt.Errorf("inlet %q: %v", inlet.Name, err)
			}
			if pair.Full == nil {
				return nil, fmt.Errorf("inlet %q: tls-certs %q has no key", inlet.Name, name)
			}
			pairs = append(pairs, pair)
		}
	}
	if len(pairs) == 0 {
		return nil, nil
	}
	return &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return pickCert(pairs, hello.ServerName), nil
		},
		ClientAuth: tls.RequestClientCert,
	}, nil
}

// pickCert chooses the pair the client hello asks for: the
// first one carrying the server name, else the first entry.
func pickCert(pairs []*Pair, serverName string) *tls.Certificate {
	if serverName != "" {
		for _, pair := range pairs {
			for _, name := range pair.Leaf.DNSNames {
				if equalFold(name, serverName) {
					return pair.Full
				}
			}
			if equalFold(pair.Leaf.Subject.CommonName, serverName) {
				return pair.Full
			}
		}
	}
	return pairs[0].Full
}

// equalFold is a hostname comparison: case-insensitive.
func equalFold(name, serverName string) bool {
	return len(name) == len(serverName) && strings.EqualFold(name, serverName)
}

// NewListener wraps a listener in TLS, the one crypto/tls
// touch outside this package.
func NewListener(l net.Listener, cfg *tls.Config) net.Listener {
	return tls.NewListener(l, cfg)
}

// ClientConfig builds the outlet transport's configuration: the
// trust pool from tls-certs, TLSDefaultCerts adding the system
// roots to it, and the presented pairs from tls-auth. The
// trust-certs flag switches tls-certs from trust anchors to
// exact leaf pins: verification skips the chain and the host
// name, and the pin becomes the whole trust decision. The
// unsafe-tls flag skips verification altogether. The returned
// config is nil when the outlet carries no TLS.
func (s *Store) ClientConfig(outlet *setup.Outlet) (*tls.Config, error) {
	flag := outlet.TrustCertsFlag()
	// A flag with no tls settings must reach its own check below,
	// not fall out as a plain config.
	if !outlet.HasTLS() && !flag {
		return nil, nil
	}
	cfg := &tls.Config{}
	if outlet.UnsafeTLSFlag() {
		// No verification runs at all: the pool built below
		// still loads the named pairs, and nothing consults it.
		cfg.InsecureSkipVerify = true
	}
	if flag {
		// A pinned leaf makes expiry, key usage, and the host
		// name moot: no attacker can produce that leaf's DER.
		// The default certs add no pin: only named pairs do.
		if len(outlet.TLSCerts) == 0 {
			return nil, fmt.Errorf("outlet %q: trust-certs needs tls-certs with pinned names", outlet.Name)
		}
		pins, err := s.NewCertSet(outlet.TLSCerts, outlet.TLSPlus)
		if err != nil {
			return nil, fmt.Errorf("outlet %q: %v", outlet.Name, err)
		}
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(state tls.ConnectionState) error {
			if _, ok := pins.InConnection(&state); !ok {
				return fmt.Errorf("tls: server certificate is not pinned")
			}
			return nil
		}
	} else if len(outlet.TLSCerts) > 0 || outlet.TLSDefaultCerts {
		// The system roots come first and the named anchors join
		// them, so TLSDefaultCerts is a merge, not a
		// replacement.
		var pool *x509.CertPool
		if outlet.TLSDefaultCerts {
			system, err := x509.SystemCertPool()
			if err != nil {
				return nil, fmt.Errorf("outlet %q: cannot read system roots: %v", outlet.Name, err)
			}
			pool = system
		} else {
			pool = x509.NewCertPool()
		}
		for _, name := range outlet.TLSCerts {
			pair, err := s.Pair(name, name, slices.Contains(outlet.TLSPlus, name))
			if err != nil {
				return nil, fmt.Errorf("outlet %q: %v", outlet.Name, err)
			}
			pool.AddCert(pair.Leaf)
		}
		cfg.RootCAs = pool
	}
	for _, name := range outlet.TLSAuth {
		pair, err := s.Pair(name, name, slices.Contains(outlet.TLSPlus, name))
		if err != nil {
			return nil, fmt.Errorf("outlet %q: %v", outlet.Name, err)
		}
		if pair.Full == nil {
			return nil, fmt.Errorf("outlet %q: tls-auth %q has no key", outlet.Name, name)
		}
		cfg.Certificates = append(cfg.Certificates, *pair.Full)
	}
	return cfg, nil
}

// CertSet is the pin set of one tls-auth list.
// Each pin stores the config name of its leaf.
type CertSet struct {
	set map[[sha256.Size]byte]string
}

// NewCertSet builds the pin set of the given names. The gens
// name those that may generate when missing; the "*" is gone
// before it gets here, split into TLSDefaultCerts at parse.
func (s *Store) NewCertSet(names, gens []string) (*CertSet, error) {
	pins := &CertSet{set: map[[sha256.Size]byte]string{}}
	for _, name := range names {
		pair, err := s.Pair(name, name, slices.Contains(gens, name))
		if err != nil {
			return nil, err
		}
		pins.set[pair.pin()] = name
	}
	return pins, nil
}

// InConnection gives the config name of the leaf that the
// connection presents. It gives no name when the set is nil,
// when the connection presents no certificate, or when no
// pin matches the leaf.
func (p *CertSet) InConnection(state *tls.ConnectionState) (string, bool) {
	if p == nil || state == nil || len(state.PeerCertificates) == 0 {
		return "", false
	}
	name, ok := p.set[sha256.Sum256(state.PeerCertificates[0].Raw)]
	return name, ok
}
