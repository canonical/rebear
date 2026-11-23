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

// Package config loads the rebear configuration file into typed
// structures. The file is cnf (see internal/cnfembed); this package
// turns its JSON translation into inlet, midlet, and outlet
// definitions with the starlark snippets held verbatim for later
// phases.
package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/canonical/rebear/internal/cnfembed"
)

// Kind tells how an inlet listens.
type Kind string

const (
	KindTCP  Kind = "tcp"
	KindUnix Kind = "unix"
)

// KeyBinding is a snippet bound to a nested key. The path holds
// one segment per nesting level; the snippet runs when the
// stream decodes the value under it. Segments stay separate so a
// wire key containing a dot cannot be confused with nesting.
type KeyBinding struct {
	Path    []string
	Snippet string
}

// BodyFilter is one direction of body filtering: a whole-body
// snippet, or key bindings. Exactly one form is set.
type BodyFilter struct {
	// Body is the whole-body snippet.
	Body string
	// Keys holds the key bindings, sorted by path.
	Keys []KeyBinding
}

// Inlet is one endpoint clients connect to and its snippets.
type Inlet struct {
	// Name is the inlet's <name> from the config.
	Name string

	// Kind is how the inlet listens, tcp or unix.
	Kind Kind

	// Address is the net address listened on: a host:port for
	// tcp - a portless one taking 80, the http default, at load -
	// or the socket path for unix, resolved to an absolute path
	// at load.
	Address string

	// URL is the routing url: its path is the mount cut from
	// request paths, its host - when present - the hostname
	// requests must carry. Nil means no constraint. Only what the
	// config says is stored; the scheme is implied: http, or
	// https when TLS is set.
	URL *url.URL

	// TLSCerts names the server certificates the listener
	// offers, from the TLS directory. Inlets sharing an address
	// must agree on the list. Non-empty turns the inlet's
	// listener into a TLS one.
	TLSCerts []string

	// TLSAuth names the client certificates the inlet accepts:
	// a client presenting none of them cannot see the inlet.
	// Non-empty requires TLSCerts.
	TLSAuth []string

	// TLSPlus holds the tls names the config prefixed with "+":
	// those may generate when missing, the rest load only.
	TLSPlus []string

	// Flags enables optional behaviors on this sniplet. The
	// known set and the contexts that accept it are validated
	// at load.
	Flags []string

	// Outlet names the outlet this inlet routes to. Empty means
	// no load-time choice: a midlet in the chain or a set_outlet
	// call must settle it.
	Outlet string

	// Midlets names the midlets the request passes through before
	// the outlet, in order. Empty means no midlet.
	Midlets []string

	// OpeningFlow is the flow opening snippet, verbatim: it runs
	// when the inlet settles on a flow, before its filters.
	OpeningFlow string

	// ClosingFlow is the flow closing snippet, verbatim: it runs
	// when the whole flow finishes, after the inlet's incoming
	// filters.
	ClosingFlow string

	// OutgoingHeaders and IncomingHeaders are the header filter
	// snippets, verbatim.
	OutgoingHeaders string
	IncomingHeaders string

	// IncomingEvent is the response event filter snippet, verbatim:
	// it runs once per event of a text/event-stream response.
	IncomingEvent string

	// OutgoingBody and IncomingBody are the body filters.
	OutgoingBody *BodyFilter
	IncomingBody *BodyFilter
}

func (inlet Inlet) Namelet() string { return inlet.Name }
func (inlet Inlet) Kindlet() string { return "inlet" }
func (inlet Inlet) HasTLS() bool    { return len(inlet.TLSAuth) > 0 || len(inlet.TLSCerts) > 0 }

// Scheme reports the scheme the inlet speaks: http, or https
// when the inlet carries tls settings.
func (inlet Inlet) Scheme() string {
	if inlet.HasTLS() {
		return "https"
	}
	return "http"
}

// Host names the endpoint: the url's host when it names one,
// else the tcp address's, else localhost - a socket or an
// unspecified address names no host of its own. The scheme's
// default port is elided; the conversation keeps the rest.
func (inlet Inlet) Host() string {
	if inlet.URL != nil && inlet.URL.Host != "" {
		return inlet.URL.Host
	}
	return trimDefaultPort(inlet.Scheme(), hostOf(inlet.Kind, inlet.Address))
}

// Outlet is one endpoint the proxy connects out to and its snippets.
type Outlet struct {
	// Name is the outlet's <name> from the config.
	Name string

	// Address is the net address dialed: a host:port for tcp - a
	// portless one taking the url's port at load, 80 on an http
	// url and 443 otherwise - or the socket path for unix,
	// resolved to an absolute path at load.
	Address string

	// Kind is how the outlet connects, tcp or unix.
	Kind Kind

	// URL is the routing url: its path prefixes the outgoing path,
	// its scheme and host - when present - become the request's.
	// Nil means the request keeps uses the address.
	URL *url.URL

	// TLSCerts names the server certificates trusted from the
	// remote, from the TLS directory - the pool they build
	// replaces the system roots.
	TLSCerts []string

	// TLSDefaultCerts adds the system roots back to that pool.
	// It comes from the "*" name in tls-certs, which names no pair
	// on disk.
	TLSDefaultCerts bool

	// TLSAuth names the client identities presented to the
	// remote.
	TLSAuth []string

	// TLSPlus holds the tls names the config prefixed with "+":
	// those may generate when missing, the rest load only.
	TLSPlus []string

	// Flags enables optional behaviors on this sniplet.
	Flags []string

	// OpeningFlow is the flow opening snippet, verbatim: it runs
	// when the outlet settles on a flow, before its filters.
	OpeningFlow string

	// ClosingFlow is the flow closing snippet, verbatim: it runs
	// when the whole flow finishes, before the midlets' and the
	// inlet's closing snippets.
	ClosingFlow string

	// OutgoingHeaders and IncomingHeaders are the header filter
	// snippets, verbatim.
	OutgoingHeaders string
	IncomingHeaders string

	// OutgoingBody and IncomingBody are the body filters.
	OutgoingBody *BodyFilter
	IncomingBody *BodyFilter

	// IncomingEvent is the response event filter snippet, verbatim:
	// it runs once per event of a text/event-stream response.
	IncomingEvent string
}

func (outlet Outlet) Namelet() string { return outlet.Name }
func (outlet Outlet) Kindlet() string { return "outlet" }

// TrustCertsFlag reports whether the outlet carries the flag
// that pins tls-certs instead of verifying their chain.
func (outlet Outlet) TrustCertsFlag() bool {
	return slices.Contains(outlet.Flags, flagTrustCerts)
}

// UnsafeTLSFlag reports whether the outlet carries the flag that
// skips certificate verification altogether.
func (outlet Outlet) UnsafeTLSFlag() bool {
	return slices.Contains(outlet.Flags, flagUnsafeTLS)
}

// HasTLS reports whether the outlet carries a TLS setting or
// the unsafe-tls flag: those force an https url and build an
// outlet client config.
func (outlet Outlet) HasTLS() bool {
	return len(outlet.TLSAuth) > 0 || len(outlet.TLSCerts) > 0 || outlet.TLSDefaultCerts || outlet.UnsafeTLSFlag()
}

// Scheme reports the scheme the outlet speaks: the url's own
// when it names one, and otherwise the implied scheme, which
// also picks the default port, 80 against 443 - https under any
// tls setting or the unsafe-tls flag; http to a local target, a
// unix socket or a loopback host not spelling 443; https to
// everything else.
func (outlet Outlet) Scheme() string {
	if outlet.URL != nil && outlet.URL.Scheme != "" {
		return outlet.URL.Scheme
	}
	if outlet.HasTLS() {
		return "https"
	}
	if outlet.Kind == KindUnix || (outlet.Kind == KindTCP && isPlainTarget(outlet.Address)) {
		return "http"
	}
	return "https"
}

// Host names the endpoint: the url's host when it names one,
// else the tcp address's, else localhost - a socket or an
// unspecified address names no host of its own. The scheme's
// default port is elided; the conversation keeps the rest.
func (outlet Outlet) Host() string {
	if outlet.URL != nil && outlet.URL.Host != "" {
		return outlet.URL.Host
	}
	return trimDefaultPort(outlet.Scheme(), hostOf(outlet.Kind, outlet.Address))
}

// isPlainTarget reports a target the outlet speaks plain http
// to: a loopback address, by IP or by the bare name, whose
// spelled port is not the https default.
func isPlainTarget(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		host, port = strings.Trim(address, "[]"), ""
	}
	if strings.EqualFold(host, "localhost") {
		return port != "443"
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return port != "443"
	}
	return false
}

// Midlet is one filter stage a request passes through between the
// inlet and the outlet, and its snippets. A midlet neither listens
// nor connects: it sits in the middle, and may override the outlet
// choice.
type Midlet struct {
	// Name is the midlet's <name> from the config.
	Name string

	// Midlets names the midlets that follow this one in the
	// chain. Empty ends the chain at this midlet.
	Midlets []string

	// Outlet names the outlet this midlet routes to, overriding
	// whatever was chosen before it. Empty leaves the choice open.
	Outlet string

	// Flags enables optional behaviors on this sniplet.
	Flags []string

	// OpeningFlow is the flow opening snippet, verbatim: it runs
	// when the midlet settles on a flow, before its filters.
	OpeningFlow string

	// ClosingFlow is the flow closing snippet, verbatim: it runs
	// when the whole flow finishes, before the inlet's closing
	// snippet.
	ClosingFlow string

	// OutgoingHeaders and IncomingHeaders are the header filter
	// snippets, verbatim.
	OutgoingHeaders string
	IncomingHeaders string

	// IncomingEvent is the response event filter snippet, verbatim:
	// it runs once per event of a text/event-stream response.
	IncomingEvent string

	// OutgoingBody and IncomingBody are the body filters.
	OutgoingBody *BodyFilter
	IncomingBody *BodyFilter
}

func (midlet *Midlet) Namelet() string { return midlet.Name }
func (midlet *Midlet) Kindlet() string { return "midlet" }
func (midlet *Midlet) HasTLS() bool    { return false }

type Sniplet interface {
	Namelet() string
	Kindlet() string
	HasTLS() bool
}

// Setup is the whole file: the inlets, midlets, and outlets, in
// file order.
type Setup struct {
	Inlets  []*Inlet
	Midlets []*Midlet
	Outlets []*Outlet
}

// rawTop mirrors the cnf file's JSON translation at the first
// level: each kind's body stays raw, its location still known,
// so a decode failure deeper in can name the section it sat in.
type rawTop struct {
	Inlets  json.RawMessage `json:"inlets"`
	Midlets json.RawMessage `json:"midlets"`
	Outlets json.RawMessage `json:"outlets"`
}

type rawInlet struct {
	Address         *string        `json:"address"`
	URL             *string        `json:"url"`
	TLSCerts        []string       `json:"tls-certs"`
	TLSAuth         []string       `json:"tls-auth"`
	Flags           []string       `json:"flags"`
	Outlet          *string        `json:"outlet"`
	Midlets         []string       `json:"midlets"`
	OpeningFlow     *string        `json:"opening-flow"`
	ClosingFlow     *string        `json:"closing-flow"`
	OutgoingHeaders *string        `json:"outgoing-headers"`
	IncomingHeaders *string        `json:"incoming-headers"`
	IncomingEvent   *string        `json:"incoming-event"`
	OutgoingBody    *rawBodyFilter `json:"outgoing-body"`
	IncomingBody    *rawBodyFilter `json:"incoming-body"`
}

type rawMidlet struct {
	Midlets         []string       `json:"midlets"`
	Outlet          *string        `json:"outlet"`
	Flags           []string       `json:"flags"`
	OpeningFlow     *string        `json:"opening-flow"`
	ClosingFlow     *string        `json:"closing-flow"`
	OutgoingHeaders *string        `json:"outgoing-headers"`
	IncomingHeaders *string        `json:"incoming-headers"`
	IncomingEvent   *string        `json:"incoming-event"`
	OutgoingBody    *rawBodyFilter `json:"outgoing-body"`
	IncomingBody    *rawBodyFilter `json:"incoming-body"`
}

type rawOutlet struct {
	Address         *string        `json:"address"`
	URL             *string        `json:"url"`
	TLSCerts        []string       `json:"tls-certs"`
	TLSAuth         []string       `json:"tls-auth"`
	Flags           []string       `json:"flags"`
	OpeningFlow     *string        `json:"opening-flow"`
	ClosingFlow     *string        `json:"closing-flow"`
	OutgoingHeaders *string        `json:"outgoing-headers"`
	IncomingHeaders *string        `json:"incoming-headers"`
	IncomingEvent   *string        `json:"incoming-event"`
	OutgoingBody    *rawBodyFilter `json:"outgoing-body"`
	IncomingBody    *rawBodyFilter `json:"incoming-body"`
}

// rawBodyFilter is a whole-body snippet (a string) or a nested
// object of key bindings.
type rawBodyFilter struct {
	body string
	keys []KeyBinding
}

func (f *rawBodyFilter) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		return json.Unmarshal(data, &f.body)
	}
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return flattenKeys(trimmed, nil, &f.keys)
	}
	return fmt.Errorf("not a snippet or a set of key bindings")
}

// flattenKeys walks the nested objects, one path segment per
// level, storing each leaf snippet under its path. Bindings come
// out sorted by path.
func flattenKeys(data []byte, prefix []string, keys *[]KeyBinding) error {
	var levels map[string]json.RawMessage
	if err := json.Unmarshal(data, &levels); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(levels)) {
		path := append(append([]string{}, prefix...), name)
		trimmed := bytes.TrimSpace(levels[name])
		if len(trimmed) > 0 && trimmed[0] == '{' {
			if err := flattenKeys(trimmed, path, keys); err != nil {
				return err
			}
			continue
		}
		var snippet string
		if err := json.Unmarshal(trimmed, &snippet); err != nil {
			return fmt.Errorf("key %q: %v", strings.Join(path, "."), err)
		}
		*keys = append(*keys, KeyBinding{Path: path, Snippet: snippet})
	}
	return nil
}

// decodeSections strictly decodes one kind's sections, so a
// decode failure names the section it happened in. The first,
// name-splitting unmarshal is loose: cnfembed has already vetted
// the structure, so it cannot realistically fail.
func decodeSections[T any](kind string, data json.RawMessage, out *map[string]T) error {
	if len(data) == 0 {
		return nil
	}
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(data, &sections); err != nil {
		return err
	}
	*out = make(map[string]T, len(sections))
	for _, name := range slices.Sorted(maps.Keys(sections)) {
		dec := json.NewDecoder(bytes.NewReader(sections[name]))
		dec.DisallowUnknownFields()
		var section T
		if err := dec.Decode(&section); err != nil {
			return fmt.Errorf("%s.%s: %v", kind, name, strings.TrimPrefix(err.Error(), "json: "))
		}
		(*out)[name] = section
	}
	return nil
}

// Load reads and validates the cnf file at path. The file's
// directory anchors relative unix socket paths.
func Load(path string) (*Setup, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data, filepath.Dir(path))
}

// Parse translates cnf text to structures. The dir anchors
// relative unix socket paths, standing in for the file's location.
func Parse(data []byte, dir string) (*Setup, error) {
	jsonData, err := cnfembed.ToJSON(data, nil)
	if err != nil {
		return nil, err
	}
	// The top level decodes strictly but shallowly: an unknown
	// top field errors here, while each kind's body stays raw for
	// the per-section decode that can name the section in errors.
	dec := json.NewDecoder(bytes.NewReader(jsonData))
	dec.DisallowUnknownFields()
	var top rawTop
	if err := dec.Decode(&top); err != nil {
		return nil, fmt.Errorf("%v", strings.TrimPrefix(err.Error(), "json: "))
	}
	var rawInlets map[string]rawInlet
	if err := decodeSections("inlets", top.Inlets, &rawInlets); err != nil {
		return nil, err
	}
	var rawMidlets map[string]rawMidlet
	if err := decodeSections("midlets", top.Midlets, &rawMidlets); err != nil {
		return nil, err
	}
	var rawOutlets map[string]rawOutlet
	if err := decodeSections("outlets", top.Outlets, &rawOutlets); err != nil {
		return nil, err
	}

	cfg := &Setup{}
	for name, raw := range rawInlets {
		inlet, err := buildInlet(name, raw, dir)
		if err != nil {
			return nil, err
		}
		cfg.Inlets = append(cfg.Inlets, inlet)
	}
	for name, raw := range rawMidlets {
		midlet, err := buildMidlet(name, raw)
		if err != nil {
			return nil, err
		}
		cfg.Midlets = append(cfg.Midlets, midlet)
	}
	for name, raw := range rawOutlets {
		outlet, err := buildOutlet(name, raw, dir)
		if err != nil {
			return nil, err
		}
		cfg.Outlets = append(cfg.Outlets, outlet)
	}
	outlets := make(map[string]*Outlet, len(cfg.Outlets))
	for _, outlet := range cfg.Outlets {
		outlets[outlet.Name] = outlet
	}
	midlets := make(map[string]*Midlet, len(cfg.Midlets))
	for _, midlet := range cfg.Midlets {
		midlets[midlet.Name] = midlet
	}
	for _, midlet := range cfg.Midlets {
		for _, name := range midlet.Midlets {
			if midlets[name] == nil {
				return nil, fmt.Errorf("midlet %q: cannot find %q midlet", midlet.Name, name)
			}
		}
		if midlet.Outlet != "" && outlets[midlet.Outlet] == nil {
			return nil, fmt.Errorf("midlet %q: cannot find %q outlet", midlet.Name, midlet.Outlet)
		}
	}
	for _, inlet := range cfg.Inlets {
		for _, name := range inlet.Midlets {
			if midlets[name] == nil {
				return nil, fmt.Errorf("inlet %q: cannot find %q midlet", inlet.Name, name)
			}
		}
		if inlet.Outlet != "" && outlets[inlet.Outlet] == nil {
			return nil, fmt.Errorf("inlet %q: cannot find %q outlet", inlet.Name, inlet.Outlet)
		}
	}
	return cfg, nil
}

func buildInlet(name string, raw rawInlet, dir string) (*Inlet, error) {
	// The "+" prefix marks the names that may generate; it is
	// trimmed here so no later code sees it.
	certs, certGens := tlsNames(raw.TLSCerts)
	auth, authGens := tlsNames(raw.TLSAuth)
	inlet := &Inlet{
		Name:            name,
		TLSCerts:        certs,
		TLSAuth:         auth,
		TLSPlus:         append(certGens, authGens...),
		Flags:           raw.Flags,
		Outlet:          str(raw.Outlet),
		Midlets:         raw.Midlets,
		OpeningFlow:     str(raw.OpeningFlow),
		ClosingFlow:     str(raw.ClosingFlow),
		OutgoingHeaders: str(raw.OutgoingHeaders),
		IncomingHeaders: str(raw.IncomingHeaders),
		IncomingEvent:   str(raw.IncomingEvent),
	}
	if err := checkTLSNames(fmt.Sprintf("inlets.%s", name), certs, auth); err != nil {
		return nil, err
	}
	if err := checkFlags(fmt.Sprintf("inlets.%s", name), inlet, raw.Flags); err != nil {
		return nil, err
	}
	// Client pins ride on server certificates: without tls-certs
	// there is no TLS listener to present them on.
	if len(raw.TLSCerts) == 0 && len(raw.TLSAuth) > 0 {
		return nil, fmt.Errorf("inlets.%s: tls-auth needs tls-certs", name)
	}
	// With TLS the implied scheme is https and an explicit http
	// url is a conflict; without it, https is.
	scheme := inlet.Scheme()
	var err error
	if inlet.URL, err = parseURL(fmt.Sprintf("inlets.%s.url", name), raw.URL, scheme); err != nil {
		return nil, err
	}
	// An omitted address takes the url's host, when it names one.
	address := ""
	if inlet.URL != nil {
		address = inlet.URL.Host
	}
	if raw.Address == nil {
		if address == "" {
			return nil, fmt.Errorf("inlets.%s: missing address", name)
		}
		inlet.Kind, inlet.Address = KindTCP, address
	} else if inlet.Kind, inlet.Address, err = parseAddress(fmt.Sprintf("inlets.%s.address", name), *raw.Address, dir); err != nil {
		return nil, err
	}
	if inlet.Kind == KindTCP {
		port := 80
		if scheme == "https" {
			port = 443
		}
		inlet.Address = withPort(inlet.Address, port)
	}
	if inlet.OutgoingBody, err = buildFilter(name, "outgoing-body", raw.OutgoingBody); err != nil {
		return nil, err
	}
	if inlet.IncomingBody, err = buildFilter(name, "incoming-body", raw.IncomingBody); err != nil {
		return nil, err
	}
	return inlet, nil
}

func buildMidlet(name string, raw rawMidlet) (*Midlet, error) {
	midlet := &Midlet{
		Name:            name,
		Midlets:         raw.Midlets,
		Outlet:          str(raw.Outlet),
		Flags:           raw.Flags,
		OpeningFlow:     str(raw.OpeningFlow),
		ClosingFlow:     str(raw.ClosingFlow),
		OutgoingHeaders: str(raw.OutgoingHeaders),
		IncomingHeaders: str(raw.IncomingHeaders),
		IncomingEvent:   str(raw.IncomingEvent),
	}
	if err := checkFlags(fmt.Sprintf("midlets.%s", name), midlet, raw.Flags); err != nil {
		return nil, err
	}
	var err error
	if midlet.OutgoingBody, err = buildFilter(name, "outgoing-body", raw.OutgoingBody); err != nil {
		return nil, err
	}
	if midlet.IncomingBody, err = buildFilter(name, "incoming-body", raw.IncomingBody); err != nil {
		return nil, err
	}
	return midlet, nil
}

func buildOutlet(name string, raw rawOutlet, dir string) (*Outlet, error) {
	// The "+" prefix marks the names that may generate; it is
	// trimmed here so no later code sees it.
	certs, certGens := tlsNames(raw.TLSCerts)
	auth, authGens := tlsNames(raw.TLSAuth)
	// The "*" spells the system roots, not a pair on disk: pull
	// it out of the names and record it on the outlet.
	n := len(certs)
	certs = slices.DeleteFunc(certs, func(name string) bool { return name == "*" })
	defaultCerts := len(certs) != n
	outlet := &Outlet{
		Name:            name,
		TLSCerts:        certs,
		TLSDefaultCerts: defaultCerts,
		TLSAuth:         auth,
		TLSPlus:         append(certGens, authGens...),
		Flags:           raw.Flags,
		OpeningFlow:     str(raw.OpeningFlow),
		ClosingFlow:     str(raw.ClosingFlow),
		OutgoingHeaders: str(raw.OutgoingHeaders),
		IncomingHeaders: str(raw.IncomingHeaders),
		IncomingEvent:   str(raw.IncomingEvent),
	}
	if err := checkTLSNames(fmt.Sprintf("outlets.%s", name), certs, auth); err != nil {
		return nil, err
	}
	if err := checkFlags(fmt.Sprintf("outlets.%s", name), outlet, raw.Flags); err != nil {
		return nil, err
	}
	// With TLS an explicit http url is a conflict; without it
	// both schemes stay open.
	schemes := []string{"http", "https"}
	if outlet.HasTLS() {
		schemes = []string{"https"}
	}
	var err error
	if outlet.URL, err = parseURL(fmt.Sprintf("outlets.%s.url", name), raw.URL, schemes...); err != nil {
		return nil, err
	}
	// An omitted address takes the url's host, when it names one.
	address := ""
	if outlet.URL != nil {
		address = outlet.URL.Host
	}
	if raw.Address == nil {
		if address == "" {
			return nil, fmt.Errorf("outlets.%s: missing address", name)
		}
		outlet.Kind, outlet.Address = KindTCP, address
	} else if outlet.Kind, outlet.Address, err = parseAddress(fmt.Sprintf("outlets.%s.address", name), *raw.Address, dir); err != nil {
		return nil, err
	}
	if outlet.Kind == KindTCP {
		// The port follows the scheme the outlet speaks.
		port := 443
		if outlet.Scheme() == "http" {
			port = 80
		}
		outlet.Address = withPort(outlet.Address, port)
	}
	if outlet.OutgoingBody, err = buildFilter(name, "outgoing-body", raw.OutgoingBody); err != nil {
		return nil, err
	}
	if outlet.IncomingBody, err = buildFilter(name, "incoming-body", raw.IncomingBody); err != nil {
		return nil, err
	}
	return outlet, nil
}

func buildFilter(section, setting string, raw *rawBodyFilter) (*BodyFilter, error) {
	if raw == nil {
		return nil, nil
	}
	if raw.body != "" && len(raw.keys) > 0 {
		return nil, fmt.Errorf("%s %s: cannot mix a whole-body snippet with key bindings", section, setting)
	}
	return &BodyFilter{Body: raw.body, Keys: raw.keys}, nil
}

// flagTrustCerts pins an outlet's server certificates
// instead of verifying their chain and host name.
const flagTrustCerts = "trust-certs"

// flagUnsafeTLS skips an outlet's certificate verification
// altogether.
const flagUnsafeTLS = "unsafe-tls"

// checkFlags validates one section's flags against the sniplet
// that carries them: the name, duplicates, the sniplet kind, and
// each flag's own context. A new flag adds a case here.
func checkFlags(section string, sniplet Sniplet, flags []string) error {
	seen := map[string]bool{}
	for _, flag := range flags {
		if err := CheckName(flag); err != nil {
			return fmt.Errorf("%s: %v", section, err)
		}
		if seen[flag] {
			return fmt.Errorf("%s: duplicate flag %q", section, flag)
		}
		seen[flag] = true
		switch flag {
		case flagTrustCerts:
			outlet, ok := sniplet.(*Outlet)
			if !ok {
				return fmt.Errorf("%s: flag %q is only valid on outlets", section, flag)
			}
			if len(outlet.TLSCerts) == 0 || outlet.TLSDefaultCerts {
				return fmt.Errorf("%s: flag %q needs tls-certs with pinned names and no default certs", section, flag)
			}
			if slices.Contains(outlet.Flags, flagUnsafeTLS) {
				return fmt.Errorf("%s: flags %q and %q cannot be used together", section, flagTrustCerts, flagUnsafeTLS)
			}
		case flagUnsafeTLS:
			outlet, ok := sniplet.(*Outlet)
			if !ok {
				return fmt.Errorf("%s: flag %q is only valid on outlets", section, flag)
			}
			if slices.Contains(outlet.Flags, flagTrustCerts) {
				return fmt.Errorf("%s: flags %q and %q cannot be used together", section, flagTrustCerts, flagUnsafeTLS)
			}
		default:
			return fmt.Errorf("%s: unknown flag %q", section, flag)
		}
	}
	return nil
}

// tlsNames splits the "+" prefix off each name: the plain
// names load only, the prefixed ones may generate, the prefix
// trimmed so no later code sees it. The "*" is never a
// generated name - buildOutlet turns it into TLSDefaultCerts.
func tlsNames(list []string) (names, gens []string) {
	for _, name := range list {
		if plain, ok := strings.CutPrefix(name, "+"); ok {
			name = plain
			if name != "*" {
				gens = append(gens, name)
			}
		}
		names = append(names, name)
	}
	return names, gens
}

// nameRule is what a sniplet name must match, and a tls name
// with it: the name lands in a filename, so no paths, dots, or
// case games.
var nameRule = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// CheckName reports whether name is a valid sniplet or tls
// name, with the rule spelled out for the refusal.
func CheckName(name string) error {
	if !nameRule.MatchString(name) {
		return fmt.Errorf("invalid name %q: must match %s", name, nameRule.String())
	}
	return nil
}

// checkTLSNames validates the tls names of one section against
// the sniplet name rule.
func checkTLSNames(section string, lists ...[]string) error {
	for _, names := range lists {
		for _, name := range names {
			if err := CheckName(name); err != nil {
				return fmt.Errorf("%s: %v", section, err)
			}
		}
	}
	return nil
}

// parseAddress splits an address into its kind and value: a
// path starting with "/" or "./" is a unix socket, anchored at
// dir when relative; anything else is a tcp host[:port], kept
// verbatim.
func parseAddress(section, value, dir string) (Kind, string, error) {
	if strings.Contains(value, "://") {
		return "", "", fmt.Errorf("%s: %q looks like a URL, use `url` field instead", section, value)
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "./") {
		if !filepath.IsAbs(value) {
			value = filepath.Join(dir, value)
		}
		return KindUnix, value, nil
	}
	if strings.Contains(value, "/") {
		return "", "", fmt.Errorf("%s: %q is not an address: unix socket path must start with \"/\" or \"./\"", section, value)
	}
	return KindTCP, value, nil
}

// withPort appends the default port to a tcp address that names
// none, so the stored address always carries it. The url is
// never touched: the port belongs to the socket, not the name
// the conversation speaks.
func withPort(address string, port int) string {
	if strings.LastIndexByte(address, ':') <= strings.LastIndexByte(address, ']') {
		return fmt.Sprintf("%s:%d", address, port)
	}
	return address
}

// localName names an endpoint that names no host of its own:
// a socket or an unspecified address speaks the local one.
const localName = "localhost"

// hostOf names the host a stored tcp address spells, else the
// local name: a socket or an unspecified address names none.
func hostOf(kind Kind, address string) string {
	if kind != KindTCP {
		return localName
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = strings.Trim(address, "[]")
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return localName
	}
	return address
}

// trimDefaultPort removes the scheme's default port from an
// endpoint's name: the port belongs to the socket, not the name
// the conversation speaks.
func trimDefaultPort(scheme, address string) string {
	if scheme == "http" {
		return strings.TrimSuffix(address, ":80")
	}
	return strings.TrimSuffix(address, ":443")
}

// parseURL parses a url setting: a /path, a full URL, or nil
// when unset. Only what the config says is stored - a scheme or
// host is never implied here, it is resolved where the url is
// used. The schemes are the ones the caller supports.
func parseURL(section string, raw *string, schemes ...string) (*url.URL, error) {
	if raw == nil || *raw == "" {
		return nil, nil
	}
	value := *raw
	var u *url.URL
	var err error
	switch {
	case strings.HasPrefix(value, "/"):
		if u, err = url.Parse(value); err != nil || u.Path == "" {
			return nil, fmt.Errorf("%s url: %q is not a valid path", section, value)
		}
	case strings.Contains(value, "://"):
		if u, err = url.Parse(value); err != nil || u.Host == "" {
			return nil, fmt.Errorf("%s url: %q is not a valid URL", section, value)
		}
		u.Host = strings.ToLower(u.Host)
		if !slices.Contains(schemes, u.Scheme) {
			return nil, fmt.Errorf("%s url: %q must be %s", section, value, strings.Join(schemes, " or "))
		}
	default:
		return nil, fmt.Errorf("%s url: %q must be a /path or a URL", section, value)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s url: %q must not hold a query or a fragment", section, value)
	}
	// A cleaned path, and a root one on a full URL: the mount and
	// the prefix both come out without a trailing slash.
	if u.Path == "" {
		u.Path = "/"
	} else {
		u.Path = path.Clean(u.Path)
	}
	return u, nil
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
