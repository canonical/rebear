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

// Package harness provides a simple HTTP client and server pair
// for driving rebear from test cases: the server plays the
// endpoint an outlet connects to and records what arrives; the
// client drives an inlet, tcp or unix.
package harness

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Response is the canned reply a Server hands out.
type Response struct {
	// Status is the HTTP status of the reply.
	Status int
	// Headers are written before the body.
	Headers map[string]string
	// Body is the reply body, unless Chunks is set.
	Body string
	// Chunks, when set, replaces Body: one slice element per
	// flush. For streaming behavior tests.
	Chunks []string
}

// RequestRecord is one request the Server received, as decoded.
// RawPath holds the path's escaped form and Query the raw query,
// both as the wire carried them.
type RequestRecord struct {
	Method  string
	Path    string
	RawPath string
	Query   string
	Host    string
	Headers map[string]string
	Body    string
	// PeerCert is the sha256 of the client certificate
	// presented, hex, or "" when none was.
	PeerCert string
	// RemoteAddr is the connection's remote address: requests
	// sharing one landed on one connection, a keep-alive.
	RemoteAddr string
}

// ResponseRecord is one response the Client received, as decoded.
type ResponseRecord struct {
	Status  int
	Headers map[string]string
	Body    string
}

// Server answers every request with a canned Response and
// records what arrives. It plays the endpoint an outlet
// connects to.
type Server struct {
	server *http.Server
	addr   string

	mu       sync.Mutex
	reply    Response
	requests []RequestRecord
}

// NewServer creates a Server replying with the given Response.
func NewServer(reply Response) *Server {
	s := &Server{reply: reply}
	s.server = &http.Server{
		Handler:      http.HandlerFunc(s.handle),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	return s
}

// handle replies with the canned Response and records the request.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("cannot read body: %v", err), http.StatusBadRequest)
		return
	}

	headers := make(map[string]string, len(r.Header))
	for name, values := range r.Header {
		headers[name] = strings.Join(values, ",")
	}
	peer := ""
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		sum := sha256.Sum256(r.TLS.PeerCertificates[0].Raw)
		peer = hex.EncodeToString(sum[:])
	}

	s.mu.Lock()
	s.requests = append(s.requests, RequestRecord{
		Method:     r.Method,
		Path:       r.URL.Path,
		RawPath:    r.URL.EscapedPath(),
		Query:      r.URL.RawQuery,
		Host:       r.Host,
		Headers:    headers,
		Body:       string(body),
		PeerCert:   peer,
		RemoteAddr: r.RemoteAddr,
	})
	reply := s.reply
	s.mu.Unlock()

	for name, value := range reply.Headers {
		w.Header().Set(name, value)
	}
	if reply.Status != 0 {
		// Zero means 200: WriteHeader's default.
		w.WriteHeader(reply.Status)
	}
	if len(reply.Chunks) > 0 {
		flusher, _ := w.(http.Flusher)
		for _, chunk := range reply.Chunks {
			_, _ = io.WriteString(w, chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		return
	}
	_, _ = io.WriteString(w, reply.Body)
}

// Start binds a free loopback port and serves in a goroutine,
// returning the base URL for an outlet.
func (s *Server) Start() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("cannot listen: %w", err)
	}
	return s.StartOn(ln)
}

// StartOn serves on a bound listener. Returns the base URL for
// tcp listeners, "" otherwise.
func (s *Server) StartOn(ln net.Listener) (string, error) {
	s.addr = ln.Addr().String()
	go func() { _ = s.server.Serve(ln) }()
	if _, ok := ln.Addr().(*net.TCPAddr); ok {
		return s.URL(), nil
	}
	return "", nil
}

// ServeOn blocks serving on the listener, for tests that own
// the serve goroutine.
func (s *Server) ServeOn(ln net.Listener) error {
	s.addr = ln.Addr().String()
	return s.server.Serve(ln)
}

// URL returns the base URL, without a trailing slash.
func (s *Server) URL() string {
	return "http://" + s.addr
}

// StartTLS binds a free loopback port and serves TLS under the
// given config, returning the https base URL.
func (s *Server) StartTLS(cfg *tls.Config) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("cannot listen: %w", err)
	}
	s.addr = ln.Addr().String()
	go func() { _ = s.server.Serve(tls.NewListener(ln, cfg)) }()
	return "https://" + s.addr, nil
}

// SetReply changes the canned reply. Safe between requests.
func (s *Server) SetReply(reply Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reply = reply
}

// Requests returns a snapshot copy, in arrival order.
func (s *Server) Requests() []RequestRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RequestRecord, len(s.requests))
	copy(out, s.requests)
	return out
}

// LastRequest returns the most recent request, or nil.
func (s *Server) LastRequest() *RequestRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return nil
	}
	req := s.requests[len(s.requests)-1]
	return &req
}

// Stop shuts the server down.
func (s *Server) Stop() error {
	return s.server.Close()
}

// Client drives an inlet. A unix:/path base dials the unix
// socket; any other base is a plain http URL.
type Client struct {
	base string
	http *http.Client
}

// NewClient creates a Client for the inlet's base URL.
func NewClient(base string) *Client {
	return newClient(base, "")
}

// NewVhostClient creates a Client for the inlet's base URL that
// sends the given host in its requests - the vhost pattern -
// while the dial still goes to the base's own address.
func NewVhostClient(base, host string) *Client {
	return newClient(base, host)
}

func newClient(base, host string) *Client {
	transport := &http.Transport{}
	requestBase := base
	network := "tcp"
	address := strings.TrimPrefix(base, "http://")
	if sock, ok := strings.CutPrefix(base, "unix:"); ok {
		network = "unix"
		address = sock
		requestBase = "http://rebear.inlet"
	}
	if host != "" {
		requestBase = "http://" + host
	}
	if network == "unix" || host != "" {
		// Only the dial goes to the address; the request still
		// speaks http to the host it names.
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}
	}
	return &Client{base: requestBase, http: &http.Client{Transport: transport, Timeout: 10 * time.Second}}
}

// TLSClientOptions describes an https drive: the dial goes to
// Addr, the requests speak Host, the server certificate is
// trusted from RootPEM, and the pair is presented when set.
type TLSClientOptions struct {
	Addr    string
	Host    string
	RootPEM []byte
	CertPEM []byte
	KeyPEM  []byte
}

// NewTLSClient creates a Client driving an https inlet.
func NewTLSClient(opts TLSClientOptions) (*Client, error) {
	cfg := &tls.Config{}
	if len(opts.RootPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(opts.RootPEM) {
			return nil, fmt.Errorf("cannot parse the root certificate")
		}
		cfg.RootCAs = pool
	}
	if len(opts.CertPEM) > 0 {
		cert, err := tls.X509KeyPair(opts.CertPEM, opts.KeyPEM)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	transport := &http.Transport{TLSClientConfig: cfg}
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", opts.Addr)
	}
	return &Client{base: "https://" + opts.Host, http: &http.Client{Transport: transport, Timeout: 10 * time.Second}}, nil
}

// Do sends one request and decodes the response.
func (c *Client) Do(method, path string, headers map[string]string, body string) (ResponseRecord, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return ResponseRecord{}, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ResponseRecord{}, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ResponseRecord{}, err
	}
	respHeaders := make(map[string]string, len(resp.Header))
	for name, values := range resp.Header {
		respHeaders[name] = strings.Join(values, ",")
	}
	return ResponseRecord{
		Status:  resp.StatusCode,
		Headers: respHeaders,
		Body:    string(respBody),
	}, nil
}
