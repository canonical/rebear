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

// Package proxy serves rebear's inlets and forwards each request to
// an outlet, running the setup's snippets as stages around the
// forwarding: the request's outlet settles after the inlet's filters,
// since a snippet may reroute it.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/canonical/rebear/internal/logging"
	"github.com/canonical/rebear/internal/notes"
	"github.com/canonical/rebear/internal/setup"
	"github.com/canonical/rebear/internal/snapshots"
	"github.com/canonical/rebear/internal/snippets"
	"github.com/canonical/rebear/internal/tls"
)

// hopHeaders are per-connection headers that must not be forwarded.
var hopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// ProxyOptions carries the inputs for one running proxy.
type ProxyOptions struct {
	// Setup is the parsed configuration file.
	Setup *setup.Setup

	// LogAt records snippet call positions in the requests'
	// log objects.
	LogAt bool

	// Listeners holds sockets inherited from a previous proxy,
	// keyed by the inlet address - normalized at parse, so the
	// same socket always keys the same entry. New takes custody:
	// on success every inherited socket is either serving in the
	// new proxy or closed. The map itself is never mutated, so a
	// failed build leaves the caller's handover intact.
	Listeners map[string]net.Listener

	// SnapshotsPath is the directory the snapshot payloads and
	// their log files land in; empty disables the feature - the
	// snapshot calls still validate and record, nothing dumps.
	SnapshotsPath string

	// Notes is the global notes store the closing-flow snippets
	// see. The caller creates it and owns it - across reloads
	// and until the final flush - handing the same store to every
	// generation. Nil disables the feature.
	Notes *notes.Store

	// TLSPath is the directory the tls-certs and tls-auth names
	// read and generate under; empty disables the settings - a
	// setup that uses them against an empty path is a build
	// error.
	TLSPath string
}

// Proxy is a built proxy: one shared listener per distinct
// listen target, serving nothing until Serve is called.
type Proxy struct {
	setup *setup.Setup

	// snapshotsPath is the dump destination, empty disabling
	// the feature for this generation.
	snapshotsPath string

	// notes is the global notes store, owned by the caller.
	notes *notes.Store

	// store reads and generates the TLS pairs, nil when the
	// setup names none.
	store     *tls.Store
	listeners map[string]*sharedListener
}

// New builds a proxy without serving it: one shared listener per
// distinct address, its handlers, and the sockets. Inlets sharing
// an address are legal when their urls - the vhost and mount -
// differ: one listener serves them all, the most specific url
// winning. A socket inherited for an address is reused as-is; one
// the new setup drops is closed and disposed. Snippets compile
// here too: a bad snippet is a startup error, leaving any
// inherited sockets open for their owner. Log output goes to
// the logging stream. Call Serve to start serving.
func New(opts *ProxyOptions) (*Proxy, error) {
	program, err := snippets.Compile(opts.Setup)
	if err != nil {
		return nil, err
	}
	program.SetLogAt(opts.LogAt)
	program.SetNotes(opts.Notes)
	store, err := tlsStore(opts.Setup, opts.TLSPath)
	if err != nil {
		return nil, err
	}
	clients, err := outletClients(opts.Setup, store)
	if err != nil {
		return nil, err
	}
	// Group the inlets by address, keeping first-seen order so
	// the startup log follows the file's.
	groups := map[string][]*setup.Inlet{}
	var keys []string
	for _, inlet := range opts.Setup.Inlets {
		if _, ok := groups[inlet.Address]; !ok {
			keys = append(keys, inlet.Address)
		}
		groups[inlet.Address] = append(groups[inlet.Address], inlet)
	}
	p := &Proxy{setup: opts.Setup, snapshotsPath: opts.SnapshotsPath, notes: opts.Notes, store: store, listeners: map[string]*sharedListener{}}
	var fresh []net.Listener
	for _, key := range keys {
		inlets := groups[key]
		if err := checkTLSAgreement(inlets); err != nil {
			p.abortBuild(fresh)
			return nil, err
		}
		master, inherited := opts.Listeners[key]
		if !inherited {
			master, err = bindListener(inlets[0])
			if err != nil {
				p.abortBuild(fresh)
				return nil, err
			}
			fresh = append(fresh, master)
		}
		sl, err := p.newSharedListener(inlets, program, clients, master)
		if err != nil {
			p.abortBuild(fresh)
			return nil, err
		}
		p.listeners[key] = sl
	}
	if len(p.listeners) == 0 {
		return nil, fmt.Errorf("no inlet to serve")
	}
	for target, ln := range opts.Listeners {
		if _, ok := p.listeners[target]; !ok {
			_ = ln.Close()
		}
	}
	return p, nil
}

// abortBuild cleans up a failed build: the servers and serving
// copies built so far, and the freshly bound masters. Inherited
// sockets stay open - their owner keeps serving on them.
func (p *Proxy) abortBuild(fresh []net.Listener) {
	for _, sl := range p.listeners {
		_ = sl.server.Close()
		_ = sl.listener.Close()
	}
	for _, master := range fresh {
		_ = master.Close()
	}
}

// Serve starts the accept loops and stops the proxy when the ctx
// ends. Infallible: everything that can fail already failed in
// New. Nothing serves before it is called, so a built proxy can
// wait for its turn - or be discarded, with Stop closing what it
// holds.
func (p *Proxy) Serve(ctx context.Context) {
	for _, sl := range p.listeners {
		go func() { _ = sl.server.Serve(sl.listener) }()
	}
	go func() {
		<-ctx.Done()
		_ = p.Stop()
	}()
}

// Retire stops the serving without closing the sockets: the
// accept loops stop, the idle connections close, and the active
// ones drain to their end in the background. The reload's stop -
// the masters live on with the next generation.
func (p *Proxy) Retire() {
	for _, sl := range p.listeners {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// The cancelled context skips the wait: Shutdown still
		// closes the serving copies and the idle connections.
		_ = sl.server.Shutdown(ctx)
		_ = sl.listener.Close()
	}
}

// Masters returns the sockets by listen target, for handing them
// to the next proxy generation. A fresh map, so the caller's
// handover is its own.
func (p *Proxy) Masters() map[string]net.Listener {
	masters := make(map[string]net.Listener, len(p.listeners))
	for target, sl := range p.listeners {
		masters[target] = sl.master
	}
	return masters
}

// Notes returns the notes store, for handing it to the next proxy
// generation and for flushing it on the way out.
func (p *Proxy) Notes() *notes.Store {
	return p.notes
}

// tlsStore builds the TLS pair store for the setup, or nil
// when nothing names a pair. A setup that names one without a
// TLS directory is a build error.
func tlsStore(cfg *setup.Setup, dir string) (*tls.Store, error) {
	storeIfTLS := func(sniplet setup.Sniplet) (store *tls.Store, err error, done bool) {
		if sniplet.HasTLS() {
			if dir == "" {
				return nil, fmt.Errorf("%s %q: tls requires a tls directory", sniplet.Kindlet(), sniplet.Namelet()), true
			}
			return tls.NewStore(dir), nil, true
		}
		return nil, nil, false
	}
	for _, inlet := range cfg.Inlets {
		if store, err, done := storeIfTLS(inlet); done {
			return store, err
		}
	}
	for _, outlet := range cfg.Outlets {
		if store, err, done := storeIfTLS(outlet); done {
			return store, err
		}
	}
	return nil, nil
}

// checkTLSAgreement holds the shared-listener rule: one socket
// speaks TLS or it does not, so the inlets on one address either
// all carry TLS or none does, and the TLS ones agree on their
// cert list.
func checkTLSAgreement(inlets []*setup.Inlet) error {
	var certs []string
	first, plain := "", ""
	for _, in := range inlets {
		if len(in.TLSCerts) == 0 {
			if plain == "" {
				plain = in.Name
			}
			continue
		}
		if first == "" {
			first = in.Name
			certs = in.TLSCerts
		} else if !slices.Equal(certs, in.TLSCerts) {
			return fmt.Errorf("inlets %q and %q: tls-certs differ on shared address %s", first, in.Name, in.Address)
		}
	}
	if first != "" && plain != "" {
		return fmt.Errorf("inlets %q and %q: cannot mix tls and plain inlets on address %s", first, plain, inlets[0].Address)
	}
	return nil
}

// bindListener binds one inlet's listener. The address is
// normalized at parse, so it names the socket as it is dialed.
func bindListener(inlet *setup.Inlet) (net.Listener, error) {
	if inlet.Kind == setup.KindUnix {
		// A stale socket from a killed run would block the bind.
		if err := os.Remove(inlet.Address); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inlet %q: cannot remove stale socket %s: %v", inlet.Name, inlet.Address, err)
		}
	}
	listen, err := net.Listen(string(inlet.Kind), inlet.Address)
	if err != nil {
		return nil, fmt.Errorf("inlet %q: cannot listen on %s: %v", inlet.Name, inlet.Address, err)
	}
	return listen, nil
}

// copyListener duplicates the master's socket into an independent
// listener: closing the copy stops that generation's accept loop
// and nothing else, and a unix copy leaves the path in place.
func copyListener(master net.Listener) (net.Listener, error) {
	file, err := master.(interface{ File() (*os.File, error) }).File()
	if err != nil {
		return nil, err
	}
	listener, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		return nil, err
	}
	return listener, nil
}

// sharedListener is one listener serving the inlets that share
// its address: each request goes to the handler whose url - the
// vhost and the mount - matches, the most specific one winning.
type sharedListener struct {
	// master is the socket itself, never served on: it survives
	// reloads, handed to the next generation.
	master net.Listener
	// listener is this generation's serving copy over the same
	// socket: closing it stops the accept loop and nothing else.
	listener net.Listener
	server   *http.Server
	// handlers holds one handler per inlet, sorted vhost-bearing
	// first - a vhost is more specific than any mount - then
	// deepest mount first so /admin/v2 wins over /admin and the
	// empty mount - the catch-all - comes last. Config order cannot
	// be the tiebreak: Parse ranges over a map.
	handlers []*handler
}

// newSharedListener builds the handlers for the inlets sharing
// one address and their serving copy over the given master. A
// repeated url - the same host and mount - is ambiguous and
// refused.
func (p *Proxy) newSharedListener(inlets []*setup.Inlet, program *snippets.Program, clients map[*setup.Outlet]*http.Client, master net.Listener) (*sharedListener, error) {
	seen := map[string]*setup.Inlet{}
	sl := &sharedListener{}
	for _, in := range inlets {
		handler, err := p.newHandler(in, program, clients)
		if err != nil {
			return nil, err
		}
		key := handler.vhost + "\x00" + handler.mount
		if other, ok := seen[key]; ok {
			return nil, fmt.Errorf("inlet %q: url %q already served by inlet %q", in.Name, handler.urlLabel(), other.Name)
		}
		seen[key] = in
		sl.handlers = append(sl.handlers, handler)
	}
	slices.SortFunc(sl.handlers, func(a, b *handler) int {
		if (a.vhost == "") != (b.vhost == "") {
			if a.vhost == "" {
				return 1
			}
			return -1
		}
		return len(b.mount) - len(a.mount)
	})
	listen, err := copyListener(master)
	if err != nil {
		return nil, fmt.Errorf("inlet %q: cannot duplicate socket: %v", inlets[0].Name, err)
	}
	// The wrap rides the serving copy, so the master and the
	// reload handover stay untouched.
	if len(inlets[0].TLSCerts) > 0 {
		serverConfig, err := p.store.ServerConfig(inlets)
		if err != nil {
			return nil, err
		}
		listen = tls.NewListener(listen, serverConfig)
	}
	sl.master = master
	sl.listener = listen
	// The server's own complaints - a TLS handshake that died
	// before a request existed, accept trouble - land on the
	// entry's own error field, the listener's address and its
	// inlets on a log object of their own.
	sl.server = &http.Server{Handler: sl, ErrorLog: log.New(&serverLog{inlets: inlets}, "", 0)}
	for _, inlet := range inlets {
		logging.Log("started listening", "inlet", inlet.Name, "address", inlet.Address)
	}
	return sl, nil
}

// serverLog is the http server's ErrorLog sink: one entry per
// line, the message under the entry's own error field, the
// listener's address and inlets on a log object of their own.
type serverLog struct {
	inlets []*setup.Inlet
}

func (l *serverLog) Write(p []byte) (int, error) {
	names := make([]string, len(l.inlets))
	for i, inlet := range l.inlets {
		names[i] = inlet.Name
	}
	logging.Write(logging.Entry{
		Error: strings.TrimSuffix(string(p), "\n"),
		Logs: []map[string]any{{
			"inlets":  names,
			"address": l.inlets[0].Address,
		}},
	})
	return len(p), nil
}

// ServeHTTP walks the handlers - the matching vhosts first, the
// deepest mount within them - serving the request on the first
// match. None matching is a 404 naming its cause: an unknown
// host when every visible inlet misses on its vhost, an unknown
// path otherwise, with the full requested url in the message.
func (sl *sharedListener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	wrongCerts, wrongPath := true, false
	for _, h := range sl.handlers {
		if _, ok := h.pins.InConnection(r.TLS); h.pins != nil && !ok {
			continue
		}
		wrongCerts = false
		if h.vhost != "" && h.vhost != host {
			continue
		}
		if trailing, ok := cutMount(h.mount, r.URL.Path); ok {
			h.serve(w, r, trailing)
			return
		}
		wrongPath = true
	}
	entry := logging.Begin(r.Method, r.URL.Path, r.URL.RawQuery)
	requested := requestedURL(r, host)
	switch {
	case wrongCerts:
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			entry.Error = "client certificate is missing"
		} else {
			entry.Error = "client certificate is unknown"
		}
	case wrongPath:
		entry.Error = fmt.Sprintf("requested path is unknown: %s", requested)
	default:
		entry.Error = fmt.Sprintf("requested host is unknown: %s", requested)
	}
	entry.Code = http.StatusNotFound
	logging.Write(*entry)
	failRequest(w, entry.Error, http.StatusNotFound)
}

// requestedURL re-dresses the request url for a no-match 404
// message: its real scheme - the server says nothing of its
// wrap, so r.TLS decides, http or https - and the client's host.
func requestedURL(r *http.Request, host string) string {
	u := *r.URL
	u.Scheme = "http"
	if r.TLS != nil {
		u.Scheme = "https"
	}
	u.Host = host
	return u.String()
}

// failRequest answers the client with a rebear failure: the
// prefix tells it apart from an origin server's reply.
func failRequest(w http.ResponseWriter, msg string, code int) {
	http.Error(w, "rebear: "+msg, code)
}

// Stop closes the server, the serving copy, and the master - the
// socket's last holder here disposes it, unix path included. Safe
// to call again: every Close is.
func (sl *sharedListener) Stop() {
	_ = sl.server.Close()
	_ = sl.listener.Close()
	_ = sl.master.Close()
}

// outgoingBody carries the request body through the outgoing stages:
// the bytes are read once, at the first sniplet whose outgoing-body
// needs them - a flow that reaches the outlet without one streams the
// body untouched.
type outgoingBody struct {
	request *http.Request
	value   *snippets.JSONValue
}

// filter runs the sniplet's outgoing-body, buffering the body on first
// need. The presence check gates the buffering only: OutgoingBody
// itself no-ops without the snippet.
func (b *outgoingBody) filter(program *snippets.Program, flow *snippets.Flow, sniplet setup.Sniplet) error {
	if !program.FiltersOutgoingBody(sniplet) {
		return nil
	}
	if b.value == nil {
		body, err := io.ReadAll(io.LimitReader(b.request.Body, snippets.JSONLimit+1))
		if err != nil {
			return bodyReadError{err}
		}
		if len(body) > snippets.JSONLimit {
			return fmt.Errorf("json body of %d bytes exceeds the %d byte limit", len(body), snippets.JSONLimit)
		}
		b.value = snippets.NewJSONValue(body)
	}
	return program.OutgoingBody(flow, sniplet, b.value)
}

// bodyReadError wraps a body read failure: the client died mid-body.
type bodyReadError struct {
	err error
}

func (e bodyReadError) Error() string {
	return fmt.Sprintf("cannot read request body: %v", e.err)
}

// snapshotFile is one requested snapshot, its name beside its
// open file: the commit pass reports the name on failures.
type snapshotFile struct {
	name string
	file *snapshots.File
}

// prepareSnapshots opens one payload file per requested name on
// a kind, keyed by the entry's own stamp - the snapshot files
// and the request's log line share it.
func (h *handler) prepareSnapshots(names []string, entry *logging.Entry, kind, mediaType string) []snapshotFile {
	if len(names) == 0 {
		return nil
	}
	files := make([]snapshotFile, 0, len(names))
	for _, name := range names {
		file := snapshots.New(h.snapshotsPath, name, entry.Time.Time, kind, mediaType)
		if file == nil {
			continue
		}
		files = append(files, snapshotFile{name: name, file: file})
	}
	return files
}

// cleanMediaType reads a header set's Content-Type down to its
// media type - lowercase, parameters dropped - or "" when unset
// or unparseable: the snapshots' extension picker.
func cleanMediaType(headers http.Header) string {
	mediaType, _, err := mime.ParseMediaType(headers.Get("Content-Type"))
	if err != nil {
		return ""
	}
	return mediaType
}

// writeSnapshots writes one buffer to every open snapshot file.
func writeSnapshots(files []snapshotFile, p []byte) {
	for _, sf := range files {
		_, _ = sf.file.Write(p)
	}
}

// commitSnapshots commits every open snapshot file, logging the
// failures on the entry: the payload crossed whole.
func commitSnapshots(files []snapshotFile, entry *logging.Entry) {
	for _, sf := range files {
		if err := sf.file.Commit(); err != nil {
			entry.Log(map[string]any{"error": fmt.Sprintf("snapshot %q: %v", sf.name, err)})
		}
	}
}

// abortSnapshots drops every open snapshot file without
// committing: the flow failed, and the .partial form - the
// partial payload - is the artifact.
func abortSnapshots(files []snapshotFile) {
	for _, sf := range files {
		sf.file.Abort()
	}
}

// streamEvent is one SSE event as read off the wire: its lines kept
// verbatim for an untouched relay, its fields parsed for the snippets.
type streamEvent struct {
	lines []string
	data  []string
	event string
	id    string
	retry string
}

// incomingStream relays a text/event-stream response, running the
// route's incoming-body snippets once per event on the event's data.
// The headers are already out: an error cuts the stream, and the
// client sees what crossed until then.
type incomingStream struct {
	program  *snippets.Program
	flow     *snippets.Flow
	sniplets []setup.Sniplet
	reader   *bufio.Reader
	writer   io.Writer
	flusher  http.Flusher
}

// isEventStream reports a text/event-stream content type.
func isEventStream(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	return strings.TrimSpace(mediaType) == "text/event-stream"
}

// streamResponse relays the response as an event stream through the
// route's snippets, answering the bytes written and the cutting
// error. The snapshots, when any, tee the lines on the way out.
func (h *handler) streamResponse(flow *snippets.Flow, outlet *setup.Outlet, resp *http.Response, midlets []*setup.Midlet, w http.ResponseWriter, inSnapshots []snapshotFile) (int64, error) {
	// The chain order is the buffered incoming one: the outlet,
	// the midlets in reverse, the inlet.
	sniplets := []setup.Sniplet{outlet}
	for i := len(midlets) - 1; i >= 0; i-- {
		sniplets = append(sniplets, midlets[i])
	}
	sniplets = append(sniplets, h.inlet)
	var writer io.Writer = w
	if len(inSnapshots) > 0 {
		writers := []io.Writer{w}
		for _, sf := range inSnapshots {
			writers = append(writers, sf.file)
		}
		writer = io.MultiWriter(writers...)
	}
	s := &incomingStream{
		program:  h.program,
		flow:     flow,
		sniplets: sniplets,
		reader:   bufio.NewReader(resp.Body),
		writer:   writer,
		flusher:  w.(http.Flusher),
	}
	return s.run()
}

// run relays event after event until the stream ends or an error
// cuts it. A trailing block of lines with no blank line behind it -
// the stream died mid-event - crosses verbatim, like the spec's own
// clients would leave it undispatched.
func (s *incomingStream) run() (int64, error) {
	var n int64
	for {
		ev, complete, err := s.readEvent()
		if err != nil {
			return n, err
		}
		if ev != nil {
			written, err := s.relay(ev, complete)
			n += written
			if err != nil {
				return n, err
			}
		}
		if !complete {
			return n, nil
		}
	}
}

// readEvent reads the lines up to the blank line, parsing the fields
// it knows and carrying the rest. The event is nil at a clean end of
// stream; complete reports the blank line that makes it dispatchable.
func (s *incomingStream) readEvent() (ev *streamEvent, complete bool, err error) {
	for {
		line, readErr := s.reader.ReadString('\n')
		if readErr != nil && readErr != io.EOF {
			return ev, false, readErr
		}
		if ev == nil {
			if readErr == io.EOF && line == "" {
				return nil, false, nil
			}
			ev = &streamEvent{}
		}
		ev.lines = append(ev.lines, line)
		trimmed := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if readErr == io.EOF {
			return ev, false, nil
		}
		if trimmed == "" {
			return ev, true, nil
		}
		if trimmed[0] == ':' {
			continue
		}
		name, val, found := strings.Cut(trimmed, ":")
		if found {
			val = strings.TrimPrefix(val, " ")
		}
		switch name {
		case "data":
			ev.data = append(ev.data, val)
		case "event":
			ev.event = val
		case "id":
			ev.id = val
		case "retry":
			ev.retry = val
		}
	}
}

// relay runs the chain over one event and writes it out. An event the
// chain never touched - the value still raw, the fields unchanged -
// crosses verbatim, original line layout included; a touched one is
// rewritten from the value and the fields.
func (s *incomingStream) relay(ev *streamEvent, complete bool) (int64, error) {
	if !complete || len(ev.data) == 0 {
		// Nothing to dispatch: comments, keep-alives, a field-only
		// block, or a dying stream's trailing lines.
		return s.write(ev.lines, true)
	}
	data := strings.Join(ev.data, "\n")
	value := snippets.NewJSONValue([]byte(data))
	event := &snippets.StreamEvent{Event: ev.event, ID: ev.id, Retry: ev.retry}
	for _, sniplet := range s.sniplets {
		if err := s.program.IncomingEvent(s.flow, sniplet, value, event); err != nil {
			return 0, err
		}
	}
	out, err := value.Bytes()
	if err != nil {
		return 0, err
	}
	if string(out) == data && event.Event == ev.event && event.ID == ev.id && event.Retry == ev.retry {
		return s.write(ev.lines, true)
	}
	// The rewritten form: the fields the event ended with, the data
	// split back into lines - the spec's own multi-line shape.
	var lines []string
	if event.Event != "" {
		lines = append(lines, "event: "+event.Event+"\n")
	}
	if event.ID != "" {
		lines = append(lines, "id: "+event.ID+"\n")
	}
	if event.Retry != "" {
		lines = append(lines, "retry: "+event.Retry+"\n")
	}
	for _, line := range strings.Split(string(out), "\n") {
		lines = append(lines, "data: "+line+"\n")
	}
	lines = append(lines, "\n")
	return s.write(lines, true)
}

// write hands the lines to the client and flushes, counting the bytes.
func (s *incomingStream) write(lines []string, flush bool) (int64, error) {
	var n int64
	for _, line := range lines {
		written, err := io.WriteString(s.writer, line)
		n += int64(written)
		if err != nil {
			return n, err
		}
	}
	if flush && s.flusher != nil {
		s.flusher.Flush()
	}
	return n, nil
}

// Stop closes every listener and server, cutting the connections.
// Safe to call again, and on a retired proxy: its masters are
// shared with the generation that took them, and closing those is
// the shutdown the caller already meant.
func (p *Proxy) Stop() error {
	for _, sl := range p.listeners {
		sl.Stop()
	}
	return nil
}

// handler proxies requests for one inlet, through the setup-wide program.
type handler struct {
	inlet   *setup.Inlet
	outlet  *setup.Outlet
	program *snippets.Program

	// clients holds one HTTP client per outlet: the shared default
	// for tcp, and a unix dialer for socket outlets.
	clients map[*setup.Outlet]*http.Client

	// midlets resolves midlet names, to seed the chain from the
	// config lists. The refs are validated at load.
	midlets map[string]*setup.Midlet

	// vhost is the inlet url's host, matched against the request's
	// Host - port included - to decide if the inlet serves it. Empty
	// matches every request.
	vhost string

	// mount is the inlet url's path, cut before routing. Matching
	// is exact: /v1 matches /v1/x, not /v10. Empty matches every
	// path.
	mount string

	// pins is the inlet's tls-auth set, nil when the inlet sets
	// no constraint: a connection presenting none of the pinned
	// leaves cannot see the inlet at all.
	pins *tls.CertSet

	// snapshotsPath is the dump destination, empty disabling
	// the feature for this generation.
	snapshotsPath string
}

// urlLabel renders the handler's url for messages: the host and
// the mount the config pinned it to.
func (h *handler) urlLabel() string {
	scheme := "http"
	if len(h.inlet.TLSCerts) > 0 {
		scheme = "https"
	}
	if h.vhost == "" {
		return h.mount
	}
	return scheme + "://" + h.vhost + h.mount
}

// outgoingURL resolves the outgoing request's URL. The scheme and
// the conversation host come from the outlet's url when it names
// them, else from the endpoint itself, per Outlet.Host - the dial
// takes the address either way. The url's path prefixes the trailing
// request path, the client's escapes preserved verbatim when the
// path was never assigned.
func (h *handler) outgoingURL(flow *snippets.Flow, outlet *setup.Outlet, r *http.Request) (url.URL, error) {
	var connect url.URL
	connect.Scheme = outlet.Scheme()
	connect.Host = outlet.Host()
	if outlet.URL != nil {
		connect.Path = outlet.URL.Path
	}
	connect.Path = strings.TrimSuffix(connect.Path, "/") + "/" + strings.TrimPrefix(flow.OutPath(), "/")
	if flow.QueryUpdated() {
		connect.RawQuery = flow.OutQuery().Encode()
	} else if r.URL.RawQuery != "" {
		connect.RawQuery = r.URL.RawQuery
	}
	if !flow.PathTouched() && r.URL.RawPath != "" {
		suffix, ok := trimEscaped(r.URL.RawPath, h.mount)
		if !ok {
			return connect, fmt.Errorf("escaped path does not decode to the request path")
		}
		prefix := ""
		if outlet.URL != nil {
			prefix = strings.TrimSuffix(outlet.URL.EscapedPath(), "/")
		}
		connect.RawPath = prefix + "/" + strings.TrimPrefix(suffix, "/")
	}
	return connect, nil
}

// newHandler builds the inlet's handler, resolving its configured
// midlet chain and outlet, and its vhost and mount. The chain
// walks the config lists - each midlet reached once, the first
// listing winning - and a midlet carrying an outlet overrides
// every choice before it.
func (p *Proxy) newHandler(inlet *setup.Inlet, program *snippets.Program, clients map[*setup.Outlet]*http.Client) (*handler, error) {
	h := &handler{inlet: inlet, program: program, clients: clients, snapshotsPath: p.snapshotsPath, midlets: map[string]*setup.Midlet{}}
	if len(inlet.TLSAuth) > 0 {
		pins, err := p.store.NewCertSet(inlet.TLSAuth, inlet.TLSPlus)
		if err != nil {
			return nil, fmt.Errorf("inlet %q: %v", inlet.Name, err)
		}
		h.pins = pins
	}
	for _, midlet := range p.setup.Midlets {
		h.midlets[midlet.Name] = midlet
	}
	for _, out := range p.setup.Outlets {
		if out.Name == inlet.Outlet {
			h.outlet = out
			break
		}
	}
	outlets := make(map[string]*setup.Outlet, len(p.setup.Outlets))
	for _, out := range p.setup.Outlets {
		outlets[out.Name] = out
	}
	reached := map[string]bool{}
	queue := append([]string(nil), inlet.Midlets...)
	for i := 0; i < len(queue); i++ {
		name := queue[i]
		if reached[name] {
			continue
		}
		reached[name] = true
		midlet := h.midlets[name]
		if midlet.Outlet != "" {
			h.outlet = outlets[midlet.Outlet]
		}
		queue = append(queue, midlet.Midlets...)
	}
	if inlet.URL != nil {
		h.vhost = vhostOf(inlet.URL.Host)
		h.mount = strings.TrimSuffix(inlet.URL.Path, "/")
	}
	return h, nil
}

// vhostOf decides the vhost a url host carries: an IP or the
// name localhost is no name a request would match - the config
// may put it there for the address alone - so it constrains
// nothing.
func vhostOf(host string) string {
	if hostname, _, err := net.SplitHostPort(host); err == nil {
		if net.ParseIP(hostname) != nil || isLocalhost(hostname) {
			return ""
		}
	} else if net.ParseIP(host) != nil || isLocalhost(host) {
		return ""
	}
	return host
}

// isLocalhost reports the bare loopback name, case-insensitive.
func isLocalhost(host string) bool {
	return strings.EqualFold(host, "localhost")
}

// The outlet connection pool tuning: how long an idle
// connection is kept, and how many idle connections each
// outlet may hold.
const (
	// idleTimeout is the keep-alive idle expiry: a connection
	// unused for this long closes itself. Zero would mean
	// no limit, which leaks sockets on quiet setups.
	idleTimeout = 5 * time.Minute
	// maxIdleConnsPerOutlet caps the idle pool per outlet - each
	// outlet dials one address, so the per-host cap is the cap.
	maxIdleConnsPerOutlet = 5
)

// outletClients builds one HTTP client per outlet, each dialing
// the outlet's address - the socket for unix, the host:port for
// tcp - while the request still speaks to the url's host. The
// TLS settings, when any, land on the transport's config.
func outletClients(cfg *setup.Setup, store *tls.Store) (map[*setup.Outlet]*http.Client, error) {
	clients := make(map[*setup.Outlet]*http.Client, len(cfg.Outlets))
	for _, outlet := range cfg.Outlets {
		network, address := string(outlet.Kind), outlet.Address
		transport := &http.Transport{
			IdleConnTimeout:     idleTimeout,
			MaxIdleConnsPerHost: maxIdleConnsPerOutlet,
		}
		if store != nil {
			clientConfig, err := store.ClientConfig(outlet)
			if err != nil {
				return nil, err
			}
			transport.TLSClientConfig = clientConfig
		}
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}
		clients[outlet] = &http.Client{Transport: transport}
	}
	return clients, nil
}

// serve runs the snippet stages around the forwarding and streams the
// response back, on the path left after the mount cut. A snippet
// failure ends the flow without forwarding. Two log records per
// request: one at the start, mostly empty, and one at the end holding
// whatever the request filled in - slow requests leave their trace
// from the first one.
func (h *handler) serve(w http.ResponseWriter, r *http.Request, trailing string) {
	entry := logging.Begin(r.Method, r.URL.Path, r.URL.RawQuery)
	// The inlet is known from the start; the midlets and the
	// outlet join once they settle.
	entry.Flow = []string{h.inlet.Name}
	if name, ok := h.pins.InConnection(r.TLS); ok {
		entry.TLSAuth = name
	}
	logging.Write(*entry)
	// The deferred call reads the entry at return time, after
	// the request filled it in.
	defer func() { logging.Write(*entry) }()

	headers := r.Header.Clone()
	stripHop(headers)
	flow := h.program.NewFlow(h.inlet, entry)
	flow.SetOut(r.Method, trailing, r.URL.RawQuery, headers)
	// The inlet's config list seeds the chain before its snippets
	// run, so an add_midlet from them extends at the end.
	for _, name := range h.inlet.Midlets {
		if err := flow.AddMidlet(h.midlets[name]); err != nil {
			h.failFlow(w, entry, nil, nil, err)
			return
		}
	}
	flow, err := h.program.OpeningFlow(flow, h.inlet, nil)
	if err != nil {
		// Nothing opened, so there is nothing to close either.
		h.failFlow(w, entry, nil, nil, err)
		return
	}
	// Registered after the deferred entry write, so it runs before
	// it: the closings' log() calls still land in the final record.
	// The locks are idempotent. The snapshot log files write after
	// the closings, both halves of a name aggregated into one.
	defer func() {
		flow.LockOut()
		flow.LockIn()
		h.program.ClosingFlow(flow)
		for _, name := range flow.SnapshotNames() {
			if err := snapshots.Log(h.snapshotsPath, name, entry.Time.Time, entry.Logs); err != nil {
				entry.Log(map[string]any{"error": fmt.Sprintf("snapshot %q: %v", name, err)})
			}
		}
	}()

	if err := h.program.OutgoingHeaders(flow, h.inlet); err != nil {
		h.failFlow(w, entry, nil, nil, err)
		return
	}

	// The body buffers at the first sniplet whose outgoing-body needs
	// it, so a flow that never filters one streams it untouched.
	body := &outgoingBody{request: r}
	if err := body.filter(h.program, flow, h.inlet); err != nil {
		h.failBody(w, entry, nil, nil, err)
		return
	}

	// The midlet chain: each popped midlet's config list joins
	// the queue before its snippets run - config first, the
	// add_midlet records after - and a midlet reached a second
	// time is skipped: it already ran.
	var midlets []*setup.Midlet
	for {
		next := flow.PopNextMidlet()
		if next == nil {
			break
		}
		for _, name := range next.Midlets {
			if err := flow.AddMidlet(h.midlets[name]); err != nil {
				// The failing list joins before its midlet settles,
				// so the route stops at the ones before it.
				h.failFlow(w, entry, midlets, nil, err)
				return
			}
		}
		midlets = append(midlets, next)
		if _, err := h.program.OpeningFlow(flow, next, nil); err != nil {
			h.failFlow(w, entry, midlets, nil, err)
			return
		}
		if err := h.program.OutgoingHeaders(flow, next); err != nil {
			h.failFlow(w, entry, midlets, nil, err)
			return
		}
		if err := body.filter(h.program, flow, next); err != nil {
			h.failBody(w, entry, midlets, nil, err)
			return
		}
	}

	// The outlet settles: set_outlet's record, else the config's.
	outlet := flow.Outlet()
	if outlet == nil {
		outlet = h.outlet
	}
	if outlet == nil {
		// No field on the inlet or a midlet, and no set_outlet
		// call: nothing routes the request.
		entry.Flow = h.routeNames(midlets, nil)
		entry.Error = "no outlet selected"
		entry.Code = http.StatusBadGateway
		failRequest(w, entry.Error, http.StatusBadGateway)
		return
	}
	if _, err := h.program.OpeningFlow(flow, outlet, nil); err != nil {
		h.failFlow(w, entry, midlets, outlet, err)
		return
	}
	if err := h.program.OutgoingHeaders(flow, outlet); err != nil {
		h.failFlow(w, entry, midlets, outlet, err)
		return
	}
	if err := body.filter(h.program, flow, outlet); err != nil {
		h.failBody(w, entry, midlets, outlet, err)
		return
	}

	// The record is exact from here: the route is known, and
	// the divergence keys land before the error check, so a
	// failed decode still logs its path.
	connect, err := h.outgoingURL(flow, outlet, r)
	entry.Flow = h.routeNames(midlets, outlet)
	entry.SetOutMethod(flow.OutMethod())
	entry.SetOutPath(connect.Path)
	entry.SetOutQuery(connect.RawQuery)
	if err != nil {
		entry.Error = err.Error()
		entry.Code = http.StatusInternalServerError
		failRequest(w, entry.Error, http.StatusInternalServerError)
		return
	}

	var reqBody io.Reader = r.Body
	var outBytes []byte
	if body.value != nil {
		outBytes, err = body.value.Bytes()
		if err != nil {
			h.failFlow(w, entry, midlets, outlet, err)
			return
		}
		reqBody = bytes.NewReader(outBytes)
	}

	// The outgoing snapshots tee here: the buffered bytes as they
	// are, or the stream on its way out.
	var outSnapshots []snapshotFile
	if names := flow.OutSnapshots(); len(names) > 0 {
		outSnapshots = h.prepareSnapshots(names, entry, "out", cleanMediaType(flow.OutHeaders()))
		defer abortSnapshots(outSnapshots)
		if body.value != nil {
			writeSnapshots(outSnapshots, outBytes)
		} else if reqBody != nil {
			var writers []io.Writer
			for _, sf := range outSnapshots {
				writers = append(writers, sf.file)
			}
			reqBody = io.TeeReader(reqBody, io.MultiWriter(writers...))
		}
	}

	req, err := http.NewRequestWithContext(r.Context(), flow.OutMethod(), connect.String(), reqBody)
	if err != nil {
		entry.Error = fmt.Sprintf("cannot build request: %v", err)
		entry.Code = http.StatusBadGateway
		failRequest(w, entry.Error, http.StatusBadGateway)
		return
	}
	req.Header = flow.OutHeaders()

	// The request leaves: from here nothing can change it. This
	// closes the outgoing span; the size is the buffered body's
	// length, or the client's own count when the body streams.
	flow.LockOut()
	var outSize int64
	if body.value != nil {
		outSize = int64(len(outBytes))
	} else if r.ContentLength > 0 {
		outSize = r.ContentLength
	}
	entry.Sent(outSize)

	resp, err := h.clients[outlet].Do(req)
	if err != nil {
		entry.Error = fmt.Sprintf("cannot reach outlet: %v", err)
		entry.Code = http.StatusBadGateway
		failRequest(w, entry.Error, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	// The request crossed whole: its snapshots commit.
	commitSnapshots(outSnapshots, entry)

	// The response seeds the incoming half: the code and the
	// headers as the outlet sent them, writable until the
	// write-back.
	stripHop(resp.Header)
	inCode := resp.StatusCode
	if err := flow.SetIn(inCode, resp.Header); err != nil {
		h.failFlow(w, entry, midlets, outlet, err)
		return
	}
	if err := h.program.IncomingHeaders(flow, outlet); err != nil {
		h.failFlow(w, entry, midlets, outlet, err)
		return
	}
	for i := len(midlets) - 1; i >= 0; i-- {
		if err := h.program.IncomingHeaders(flow, midlets[i]); err != nil {
			h.failFlow(w, entry, midlets, outlet, err)
			return
		}
	}
	if err := h.program.IncomingHeaders(flow, h.inlet); err != nil {
		h.failFlow(w, entry, midlets, outlet, err)
		return
	}
	// The outlet is settled here, so the tests are exact: the midlets
	// that ran are known too. The lanes split on the content type:
	// an event stream takes the event chain, everything else the
	// body chain.
	evented := h.program.FiltersIncomingEvent(outlet) || h.program.FiltersIncomingEvent(h.inlet)
	for _, midlet := range midlets {
		evented = evented || h.program.FiltersIncomingEvent(midlet)
	}
	filtered := h.program.FiltersIncomingBody(outlet) || h.program.FiltersIncomingBody(h.inlet)
	for _, midlet := range midlets {
		filtered = filtered || h.program.FiltersIncomingBody(midlet)
	}
	if evented && isEventStream(flow.InHeaders().Get("Content-Type")) {
		// A stream has no length, and the headers go out before
		// the first event: a snippet failure mid-stream cuts the
		// stream instead of answering a status. The snapshots tee
		// every event line on its way to the client.
		flow.LockIn()
		inHeaders := flow.InHeaders()
		inHeaders.Del("Content-Length")
		copyHeaders(w.Header(), inHeaders)
		w.WriteHeader(flow.InCode())
		entry.Code = int64(flow.InCode())
		entry.SetInCode(int64(inCode))
		if names := flow.InSnapshots(); len(names) > 0 {
			inSnapshots := h.prepareSnapshots(names, entry, "in", cleanMediaType(inHeaders))
			defer abortSnapshots(inSnapshots)
			n, err := h.streamResponse(flow, outlet, resp, midlets, w, inSnapshots)
			if err != nil {
				entry.Error = err.Error()
			} else {
				commitSnapshots(inSnapshots, entry)
			}
			entry.Done(n)
			return
		}
		n, err := h.streamResponse(flow, outlet, resp, midlets, w, nil)
		if err != nil {
			entry.Error = err.Error()
		}
		entry.Done(n)
		return
	}
	// The response value holds the body across the incoming stages,
	// the same way the request's held it across the outgoing ones.
	var respValue *snippets.JSONValue
	if filtered {
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, snippets.JSONLimit+1))
		if err != nil {
			entry.Error = fmt.Sprintf("cannot read response body: %v", err)
			entry.Code = http.StatusBadGateway
			entry.SetInCode(int64(inCode))
			failRequest(w, entry.Error, http.StatusBadGateway)
			return
		}
		if len(respBody) > snippets.JSONLimit {
			h.failFlow(w, entry, midlets, outlet, fmt.Errorf("json body of %d bytes exceeds the %d byte limit", len(respBody), snippets.JSONLimit))
			return
		}
		respValue = snippets.NewJSONValue(respBody)
		if err := h.program.IncomingBody(flow, outlet, respValue); err != nil {
			h.failFlow(w, entry, midlets, outlet, err)
			return
		}
		for i := len(midlets) - 1; i >= 0; i-- {
			if err := h.program.IncomingBody(flow, midlets[i], respValue); err != nil {
				h.failFlow(w, entry, midlets, outlet, err)
				return
			}
		}
		if err := h.program.IncomingBody(flow, h.inlet, respValue); err != nil {
			h.failFlow(w, entry, midlets, outlet, err)
			return
		}
	}
	// The response leaves: from here nothing can change it. The
	// bytes are produced before the headers go out, so an encode
	// failure still answers with a clean error status.
	var respBytes []byte
	if filtered {
		respBytes, err = respValue.Bytes()
		if err != nil {
			h.failFlow(w, entry, midlets, outlet, err)
			return
		}
	}
	var inSnapshots []snapshotFile
	if names := flow.InSnapshots(); len(names) > 0 {
		inSnapshots = h.prepareSnapshots(names, entry, "in", cleanMediaType(flow.InHeaders()))
		defer abortSnapshots(inSnapshots)
	}
	flow.LockIn()
	inHeaders := flow.InHeaders()
	if filtered {
		// A re-encoded body no longer matches the length the
		// outlet declared.
		inHeaders.Del("Content-Length")
	}
	copyHeaders(w.Header(), inHeaders)
	w.WriteHeader(flow.InCode())
	entry.Code = int64(flow.InCode())
	entry.SetInCode(int64(inCode))
	// One copy for every lane: the value's bytes or the outlet's
	// stream, to the client, the snapshots tee'd on the way - a
	// clean copy commits them, the count closes the incoming span.
	var src io.Reader = resp.Body
	if filtered {
		src = bytes.NewReader(respBytes)
	}
	var writer io.Writer = w
	if len(inSnapshots) == 1 {
		src = io.TeeReader(src, inSnapshots[0].file)
	} else if len(inSnapshots) > 1 {
		writers := make([]io.Writer, 0, len(inSnapshots))
		for _, sf := range inSnapshots {
			writers = append(writers, sf.file)
		}
		src = io.TeeReader(src, io.MultiWriter(writers...))
	}
	n, copyErr := io.Copy(writer, src)
	if copyErr == nil {
		commitSnapshots(inSnapshots, entry)
	}
	entry.Done(n)
}

// failBody answers a body stage failure: a read failure is the
// client's (400), anything else goes to failFlow.
func (h *handler) failBody(w http.ResponseWriter, entry *logging.Entry, midlets []*setup.Midlet, outlet *setup.Outlet, err error) {
	var readErr bodyReadError
	if !errors.As(err, &readErr) {
		h.failFlow(w, entry, midlets, outlet, err)
		return
	}
	entry.Flow = h.routeNames(midlets, outlet)
	entry.Error = readErr.Error()
	entry.Code = http.StatusBadRequest
	failRequest(w, entry.Error, http.StatusBadRequest)
}

// failFlow answers a snippet failure. A fail() rejection carries
// its code and body to the client; any other snippet error answers
// 500 with the message. The midlets and the outlet are the settled
// ones, or nil while the flow has not reached those stages.
func (h *handler) failFlow(w http.ResponseWriter, entry *logging.Entry, midlets []*setup.Midlet, outlet *setup.Outlet, err error) {
	if outlet == nil {
		outlet = h.outlet
	}
	msg := err.Error()
	var failErr *snippets.FailError
	if errors.As(err, &failErr) && failErr.Code != 0 {
		msg = fmt.Sprintf("%s (%d)", msg, failErr.Code)
	}
	entry.Flow = h.routeNames(midlets, outlet)
	entry.Error = msg
	if !errors.As(err, &failErr) {
		entry.Code = http.StatusInternalServerError
		failRequest(w, err.Error(), http.StatusInternalServerError)
		return
	}
	status := failErr.Code
	if status == 0 {
		status = http.StatusInternalServerError
	}
	entry.Code = int64(status)
	if failErr.BodyJSON {
		w.Header().Set("Content-Type", "application/json")
	} else if failErr.Body == "" {
		failErr.Body = failErr.Message
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, failErr.Body)
}

// routeNames lists the request's route for the log: the inlet, the
// midlet chain, and the outlet - or the inlet alone while the outlet
// is unknown.
func (h *handler) routeNames(midlets []*setup.Midlet, outlet *setup.Outlet) []string {
	route := []string{h.inlet.Name}
	for _, midlet := range midlets {
		route = append(route, midlet.Name)
	}
	if outlet != nil {
		route = append(route, outlet.Name)
	}
	return route
}

// cutMount removes the mount prefix, reporting whether the path is under
// the mount. The remainder keeps its leading slash.
func cutMount(mount, path string) (string, bool) {
	if mount == "" {
		return path, true
	}
	if path == mount {
		return "/", true
	}
	if strings.HasPrefix(path, mount+"/") {
		return path[len(mount):], true
	}
	return "", false
}

// trimEscaped cuts the decoded mount prefix from the escaped raw URL.
func trimEscaped(raw, mount string) (string, bool) {
	i := 0
	for n := 0; n < len(mount); n++ {
		if i >= len(raw) {
			return "", false
		}
		if raw[i] == '%' {
			if i+2 >= len(raw) {
				return "", false
			}
			hi, lo := unhex(raw[i+1]), unhex(raw[i+2])
			if hi < 0 || lo < 0 || byte(hi<<4|lo) != mount[n] {
				return "", false
			}
			i += 3
		} else {
			if raw[i] != mount[n] {
				return "", false
			}
			i++
		}
	}
	return raw[i:], true
}

// unhex values a hex digit, or -1 for any other byte.
func unhex(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c && c <= 'f':
		return int(c - 'a' + 10)
	case 'A' <= c && c <= 'F':
		return int(c - 'A' + 10)
	}
	return -1
}

// copyHeaders copies every header from src to dst.
func copyHeaders(dst, src http.Header) {
	for name, values := range src {
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

// stripHop removes the per-connection headers from the set.
func stripHop(h http.Header) {
	for _, name := range hopHeaders {
		h.Del(name)
	}
}
