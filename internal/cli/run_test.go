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

package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/canonical/rebear/internal/harness"
	"github.com/canonical/rebear/internal/secrets"
)

// syncWriter is a strings.Builder safe to read while another
// goroutine writes.
type syncWriter struct {
	mu sync.Mutex
	sb strings.Builder
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sb.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sb.String()
}

// runServe starts Run with a cancellable context and returns its
// output once the inlets are listening, then lets the cancellation
// close them at cleanup.
func runServe(t *testing.T, argv []string, setupPath string) (string, error) {
	t.Helper()
	stdout := &syncWriter{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{
			Argv:      argv,
			Stdout:    stdout,
			Stderr:    stdout,
			SetupPath: setupPath,
		})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stdout.String(), `"msg":"started listening"`) {
		select {
		case err := <-done:
			// Run ended before listening: surface whatever it said.
			return stdout.String(), err
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			return stdout.String(), <-done
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	return stdout.String(), <-done
}

func run(t *testing.T, argv []string, setupPath string) (string, error) {
	t.Helper()
	var stdout, stderr strings.Builder
	err := Run(context.Background(), RunOptions{
		Argv:      argv,
		Stdout:    &stdout,
		Stderr:    &stderr,
		SetupPath: setupPath,
	})
	return stdout.String(), err
}

func TestRunHelp(t *testing.T) {
	// -h prints the usage text to stderr and exits cleanly.
	var stdout, stderr strings.Builder
	err := Run(context.Background(), RunOptions{
		Argv:   []string{"-h"},
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stderr.String(), "Usage: rebear") {
		t.Errorf("stderr = %q, want usage text", stderr.String())
	}
}

// reindent strips each line up to and including its first "|",
// keeping the suffix as the line, and drops lines without one. It
// lets a cnf sample sit pipe-prefixed and indented inside a
// function while the content keeps cnf's column-consistent form.
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

// writeSetupAt writes the setup to the given path.
func writeSetupAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("cannot create setup dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("cannot write setup: %v", err)
	}
}

// writeSetupFile puts a setup in a temp file and returns its path.
func writeSetupFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rebear.cnf")
	writeSetupAt(t, path, content)
	return path
}

func TestRunSetupPathOption(t *testing.T) {
	// RunOptions.SetupPath selects the setup file, for callers
	// like tests that configure the run programmatically.
	sock := filepath.Join(t.TempDir(), "option.sock")
	path := writeSetupFile(t, reindent(`
		|inlets.local:
		|    address: "`+sock+`"
		|    outlet: "remote"
		|outlets.remote:
		|    address: "127.0.0.1:9"
		|    url: "http://127.0.0.1:9"
		`))
	stdout, err := runServe(t, nil, path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout, `"msg":"started listening"`) {
		t.Errorf("stdout = %q, want a listening line", stdout)
	}
}

// setEnv sets an environment variable for the duration of the test.
func setEnv(t *testing.T, key, value string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	os.Setenv(key, value)
	t.Cleanup(func() {
		if had {
			os.Setenv(key, old)
		} else {
			os.Unsetenv(key)
		}
	})
}

// unsetEnv clears an environment variable for the duration of
// the test.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	os.Unsetenv(key)
	t.Cleanup(func() {
		if had {
			os.Setenv(key, old)
		}
	})
}

// setupContent returns a minimal setup listening on the given socket.
func setupContent(sock string) string {
	return reindent(`
		|inlets.local:
		|    address: "` + sock + `"
		|    outlet: "remote"
		|outlets.remote:
		|    address: "127.0.0.1:9"
		|    url: "http://127.0.0.1:9"
		`)
}

func TestRunSetupHomeEnv(t *testing.T) {
	// $REBEAR_HOME holds a directory whose rebear.cnf is the setup.
	sock := filepath.Join(t.TempDir(), "home.sock")
	dir := t.TempDir()
	writeSetupAt(t, filepath.Join(dir, "rebear.cnf"), setupContent(sock))
	setEnv(t, "REBEAR_HOME", dir)
	unsetEnv(t, "XDG_CONFIG_HOME")

	stdout, err := runServe(t, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout, `"msg":"started listening"`) {
		t.Errorf("stdout = %q, want a listening line", stdout)
	}
}

func TestRunSetupXDGConfigHome(t *testing.T) {
	// Without $REBEAR_HOME, the setup comes from
	// $XDG_CONFIG_HOME/rebear/rebear.cnf.
	sock := filepath.Join(t.TempDir(), "xdg.sock")
	dir := t.TempDir()
	writeSetupAt(t, filepath.Join(dir, "rebear", "rebear.cnf"), setupContent(sock))
	unsetEnv(t, "REBEAR_HOME")
	setEnv(t, "XDG_CONFIG_HOME", dir)

	stdout, err := runServe(t, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout, `"msg":"started listening"`) {
		t.Errorf("stdout = %q, want a listening line", stdout)
	}
}

func TestRunSetupHomeDirFallback(t *testing.T) {
	// Without $REBEAR_HOME or $XDG_CONFIG_HOME, the setup comes
	// from $HOME/.config/rebear/rebear.cnf.
	sock := filepath.Join(t.TempDir(), "fallback.sock")
	home := t.TempDir()
	writeSetupAt(t, filepath.Join(home, ".config", "rebear", "rebear.cnf"), setupContent(sock))
	unsetEnv(t, "REBEAR_HOME")
	unsetEnv(t, "XDG_CONFIG_HOME")
	setEnv(t, "HOME", home)

	stdout, err := runServe(t, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout, `"msg":"started listening"`) {
		t.Errorf("stdout = %q, want a listening line", stdout)
	}
}

func TestRunSetupHomeBeatsXDG(t *testing.T) {
	// $REBEAR_HOME wins over $XDG_CONFIG_HOME: its missing path is
	// the one named in the error.
	setEnv(t, "REBEAR_HOME", "/nonexistent/rebear-home")
	setEnv(t, "XDG_CONFIG_HOME", "/nonexistent/xdg")
	_, err := run(t, nil, "")
	want := "/nonexistent/rebear-home/rebear.cnf"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %s mentioned", err, want)
	}
}

func TestRunMissingSetupInConfigDir(t *testing.T) {
	// A missing setup at the XDG default fails with the resolved
	// path in the message.
	unsetEnv(t, "REBEAR_HOME")
	dir := t.TempDir()
	setEnv(t, "XDG_CONFIG_HOME", dir)
	_, err := run(t, nil, "")
	want := filepath.Join(dir, "rebear", "rebear.cnf")
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %s mentioned", err, want)
	}
}

func TestRunBadSetup(t *testing.T) {
	// A setup that parses as cnf but is invalid (an outlet without
	// connect) fails with the file path and the problem named.
	path := writeSetupFile(t, reindent(`
		|outlets.bad:
		|    opening-flow: "x"
	`))
	_, err := run(t, nil, path)
	if err == nil || !strings.Contains(err.Error(), "missing address") || !strings.Contains(err.Error(), path) {
		t.Fatalf("error = %v, want missing address and path mentioned", err)
	}
}

func TestRunBadArguments(t *testing.T) {
	// A parse error surfaces from Run unchanged.
	_, err := run(t, []string{"bogus"}, "")
	if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("error = %v, want unexpected argument", err)
	}
}

func TestRunInletWithoutOutlet(t *testing.T) {
	// No outlet is selected by name match: an inlet with no
	// outlet field loads and listens even with no same-name
	// outlet, and its requests fail at routing time instead.
	sock := filepath.Join(t.TempDir(), "nowhere.sock")
	path := writeSetupFile(t, reindent(`
		|inlets.local:
		|    address: "`+sock+`"
		|outlets.remote:
		|    address: "127.0.0.1:9"
		|    url: "http://127.0.0.1:9"
		`))
	stdout, err := runServe(t, nil, path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout, `"msg":"started listening"`) {
		t.Errorf("stdout = %q, want a listening line", stdout)
	}
}

func TestRunInletWithUnknownOutlet(t *testing.T) {
	// An inlet naming an outlet that does not exist fails at load.
	path := writeSetupFile(t, reindent(`
		|inlets.local:
		|    address: "/nowhere.sock"
		|    outlet: "nope"
		`))
	_, err := run(t, nil, path)
	if err == nil || !strings.Contains(err.Error(), `cannot find "nope" outlet`) {
		t.Fatalf("error = %v, want cannot find nope outlet", err)
	}
}

// startReloadable runs Run on a setup file it may rewrite, and
// returns its output, the cancellation, and the result channel.
// The caller waits for the listening lines before signaling.
func startReloadable(t *testing.T, path string) (*syncWriter, context.CancelFunc, <-chan error) {
	t.Helper()
	stdout := &syncWriter{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{
			Stdout:    stdout,
			Stderr:    stdout,
			SetupPath: path,
		})
	}()
	return stdout, cancel, done
}

// waitFor polls the output until it holds the marker, failing
// on the deadline.
func waitFor(t *testing.T, stdout *syncWriter, done <-chan error, marker string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stdout.String(), marker) {
		select {
		case err := <-done:
			t.Fatalf("Run ended before %q: %v", marker, err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in %q", marker, stdout.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// remote starts a harness server playing the outlet's
// endpoint, and returns its base URL.
func remote(t *testing.T, reply harness.Response) string {
	t.Helper()
	s := harness.NewServer(reply)
	url, err := s.Start()
	if err != nil {
		t.Fatalf("cannot start upstream: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	return url
}

// reloadCnf renders a one-inlet setup over the given socket and
// outlet URL.
func reloadCnf(sock, remoteURL string) string {
	return reindent(`
		|inlets.local:
		|    address: "` + sock + `"
		|    outlet: "remote"
		|outlets.remote:
		|    address: "` + strings.TrimPrefix(remoteURL, "http://") + `"
		|    url: "` + remoteURL + `"
		`)
}

func TestRunSIGHUPReload(t *testing.T) {
	// SIGHUP re-reads the setup file: the new outlet answers on
	// the same socket, and the log carries the reload line.
	remote1URL := remote(t, harness.Response{Status: 200, Body: "old"})
	remote2URL := remote(t, harness.Response{Status: 200, Body: "new"})
	sock := filepath.Join(t.TempDir(), "sighup.sock")
	path := writeSetupFile(t, reloadCnf(sock, remote1URL))

	stdout, cancel, done := startReloadable(t, path)
	waitFor(t, stdout, done, `"msg":"started listening"`)

	writeSetupAt(t, path, reloadCnf(sock, remote2URL))
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("cannot signal: %v", err)
	}
	waitFor(t, stdout, done, `"msg":"SIGHUP - reloading configuration"`)
	waitFor(t, stdout, done, `"msg":"SIGHUP - configuration reloaded"`)

	resp, err := harness.NewClient("unix:"+sock).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error after reload: %v", err)
	}
	if resp.Body != "new" {
		t.Errorf("body = %q, want the new outlet's", resp.Body)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunSIGHUPBrokenSetup(t *testing.T) {
	// A reload from a broken file changes nothing: the current
	// setup keeps serving, and the failure goes to the log.
	remote1URL := remote(t, harness.Response{Status: 200, Body: "old"})
	sock := filepath.Join(t.TempDir(), "broken.sock")
	path := writeSetupFile(t, reloadCnf(sock, remote1URL))

	stdout, cancel, done := startReloadable(t, path)
	waitFor(t, stdout, done, `"msg":"started listening"`)

	writeSetupAt(t, path, reindent(`
		|inlets.local:
		|    outlet: "remote"
		`))
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("cannot signal: %v", err)
	}
	waitFor(t, stdout, done, `"msg":"SIGHUP - reloading configuration"`)
	waitFor(t, stdout, done, `"msg":"SIGHUP - cannot reload configuration"`)

	resp, err := harness.NewClient("unix:"+sock).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error after failed reload: %v", err)
	}
	if resp.Body != "old" {
		t.Errorf("body = %q, want the old outlet still serving", resp.Body)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunAddTLS(t *testing.T) {
	// --add-tls writes the named pairs under the tls directory
	// and exits without serving; the confirmation names the real
	// path, home untouched since the directory sits outside it;
	// an existing name or a bad one is refused, and nothing else
	// is touched.
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(home, "tls")

	var stdout strings.Builder
	err := Run(context.Background(), RunOptions{
		Argv:   []string{"--add-tls", "foo,bar"},
		Stdout: &stdout,
		Stderr: &stdout,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, name := range []string{"foo", "bar"} {
		for _, suffix := range []string{"cert", "key"} {
			if _, err := os.Stat(filepath.Join(dir, name+"."+suffix)); err != nil {
				t.Errorf("%s.%s: %v", name, suffix, err)
			}
		}
	}
	if !strings.Contains(stdout.String(), "tls: generated "+filepath.Join(dir, "foo.{cert,key}")) {
		t.Errorf("stdout = %q, want the generated lines", stdout.String())
	}

	err = Run(context.Background(), RunOptions{
		Argv:   []string{"--add-tls", "foo"},
		Stdout: &stdout,
		Stderr: &stdout,
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v, want already exists", err)
	}

	err = Run(context.Background(), RunOptions{
		Argv:   []string{"--add-tls", "Bad/Name"},
		Stdout: &stdout,
		Stderr: &stdout,
	})
	if err == nil || !strings.Contains(err.Error(), "must match") {
		t.Errorf("error = %v, want the name rule", err)
	}
}

func TestRunAddTLSUsedBySetup(t *testing.T) {
	// A pair written by --add-tls serves a real setup: a plain
	// tls name - no +, load only - loads the generated files and
	// the https round trip goes through to the outlet.
	remoteURL := remote(t, harness.Response{Status: 200, Body: "hi"})
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)

	var stdout strings.Builder
	if err := Run(context.Background(), RunOptions{
		Argv:   []string{"--add-tls", "svc"},
		Stdout: &stdout,
		Stderr: &stdout,
	}); err != nil {
		t.Fatalf("cannot generate pair: %v", err)
	}
	certPEM, err := os.ReadFile(filepath.Join(home, "tls", "svc.cert"))
	if err != nil {
		t.Fatalf("cannot read generated cert: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot grab a port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	path := writeSetupFile(t, reindent(`
		|inlets.local:
		|    address: "`+addr+`"
		|    tls-certs: ["svc"]
		|    outlet: "remote"
		|outlets.remote:
		|    address: "`+strings.TrimPrefix(remoteURL, "http://")+`"
		|    url: "`+remoteURL+`"
		`))
	serveOut, cancel, done := startReloadable(t, path)
	waitFor(t, serveOut, done, `"msg":"started listening"`)

	// The generated leaf carries the pair's name as CN, so the
	// client speaks that name - and its SNI picks the cert.
	client, err := harness.NewTLSClient(harness.TLSClientOptions{Addr: addr, Host: "svc", RootPEM: certPEM})
	if err != nil {
		t.Fatalf("cannot create client: %v", err)
	}
	resp, err := client.Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 || resp.Body != "hi" {
		t.Errorf("response = %d %q, want the outlet's", resp.Status, resp.Body)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// zeroSalt is the one salt the tests seal under, sharing the
// derivation across a whole directory.
var zeroSalt = base64.StdEncoding.EncodeToString(make([]byte, 24))

// addSecret seals the named secret through the one-shot.
func addSecret(t *testing.T, name, salt, password, plain string) {
	t.Helper()
	if err := Run(context.Background(), RunOptions{
		Argv:   []string{"--add-secret", name, "--salt", salt},
		Stdin:  strings.NewReader(password + "\n" + plain + "\n"),
		Stdout: io.Discard,
		Stderr: io.Discard,
	}); err != nil {
		t.Fatalf("cannot seal %s: %v", name, err)
	}
}

// sealBody reads the seal's own body as fields.
func sealBody(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read the seal: %v", err)
	}
	var fields map[string]string
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("cannot decode the seal: %v", err)
	}
	return fields
}

// startReloadableIn starts Run the way startReloadable does,
// with the given arguments, reading the asks from the given
// stdin.
func startReloadableIn(t *testing.T, argv []string, path string, stdin io.Reader) (*syncWriter, context.CancelFunc, <-chan error) {
	t.Helper()
	stdout := &syncWriter{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{
			Argv:      argv,
			Stdin:     stdin,
			Stdout:    stdout,
			Stderr:    stdout,
			SetupPath: path,
		})
	}()
	return stdout, cancel, done
}

// secretToolFake puts a fake secret-tool on the PATH for the
// test. Every invocation's arguments land in the args file,
// one per line, and a stored password lands in the stored
// file, the bytes stdin carried. The lookup answers the home
// it knows with the map's value - a home the map leaves out
// answers nothing and exits 1, the pair the real tool gives
// for the missing entry - and the store answers nothing and
// succeeds. The lookup prints the password with its newline,
// the trim on rebear's side.
func secretToolFake(t *testing.T, homePasswords map[string]string) (args, stored string) {
	t.Helper()
	dir := t.TempDir()
	args = filepath.Join(dir, "args")
	stored = filepath.Join(dir, "stored")
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	fmt.Fprintf(&script, "for arg in \"$@\"; do echo \"$arg\" >> %q; done\n", args)
	fmt.Fprintf(&script, "if [ \"$1\" = store ]; then cat > %q; exit 0; fi\n", stored)
	script.WriteString("case \"$5\" in\n")
	for home, password := range homePasswords {
		fmt.Fprintf(&script, "\t%q) echo %q; exit 0 ;;\n", home, password)
	}
	script.WriteString("esac\nexit 1\n")
	if err := os.WriteFile(filepath.Join(dir, "secret-tool"), []byte(script.String()), 0o755); err != nil {
		t.Fatalf("cannot write the fake: %v", err)
	}
	// The fake leads the PATH but does not replace it: its
	// store branch calls cat, an external binary the lookup's
	// builtin echo never needs.
	t.Setenv("PATH", dir+string(filepath.ListSeparator)+os.Getenv("PATH"))
	return args, stored
}

// secretToolFailingFake puts a fake secret-tool on the PATH
// that writes the complaint to stderr and exits 1: the
// failed-exec cell, the tool's own complaints self-named.
func secretToolFailingFake(t *testing.T, complaint string) {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\necho %q >&2\nexit 1\n", complaint)
	if err := os.WriteFile(filepath.Join(dir, "secret-tool"), []byte(script), 0o755); err != nil {
		t.Fatalf("cannot write the fake: %v", err)
	}
	t.Setenv("PATH", dir)
}

// secretToolArgs reads the lookups the fake recorded, one
// argument per line, as one run's five.
func secretToolArgs(t *testing.T, args string) []string {
	t.Helper()
	data, err := os.ReadFile(args)
	if err != nil {
		t.Fatalf("cannot read the lookup args: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// secretCnf renders a setup whose outlet reads the named
// secret into the authorization header.
func secretCnf(sock, remoteURL, name string) string {
	return reindent(`
		|inlets.local:
		|    address: "` + sock + `"
		|    outlet: "remote"
		|outlets.remote:
		|    address: "` + strings.TrimPrefix(remoteURL, "http://") + `"
		|    url: "` + remoteURL + `"
		|    outgoing-headers:
		|        """
		|        headers["Authorization"] = "Bearer " + secrets["` + name + `"]
		|        """
		`)
}

func TestRunAddSecretSealsAndRotates(t *testing.T) {
	// --add-secret seals the two piped lines - the \r of a
	// CRLF pipe trims - writes the file beside nothing else,
	// and confirms with the real path, home untouched since
	// the file sits outside it; a second run overwrites the
	// same name and keeps the directory's salt: the rotation
	// path.
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(home, "secrets")

	var stdout strings.Builder
	if err := Run(context.Background(), RunOptions{
		Argv:   []string{"--add-secret", "api"},
		Stdin:  strings.NewReader("pw\r\ntok-one\r\n"),
		Stdout: &stdout,
		Stderr: &stdout,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	path := filepath.Join(dir, "api.json")
	if !strings.Contains(stdout.String(), "secrets: wrote "+path) {
		t.Errorf("stdout = %q, want the confirmation naming %s", stdout.String(), path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("cannot stat the seal: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 0600", perm)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("cannot stat the secrets dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %o, want 0700", perm)
	}
	before := sealBody(t, path)

	if err := Run(context.Background(), RunOptions{
		Argv:   []string{"--add-secret", "api"},
		Stdin:  strings.NewReader("pw\ntok-two\n"),
		Stdout: &stdout,
		Stderr: &stdout,
	}); err != nil {
		t.Fatalf("cannot rotate: %v", err)
	}
	after := sealBody(t, path)
	if after["salt"] != before["salt"] {
		t.Errorf("salt = %q, want the directory's own %q kept", after["salt"], before["salt"])
	}

	sealed, err := secrets.Scan(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	opened, err := secrets.Open(sealed, "pw")
	if err != nil {
		t.Fatalf("cannot open: %v", err)
	}
	if opened["api"] != "tok-two" {
		t.Errorf("api = %q, want the rotated plaintext", opened["api"])
	}
}

func TestRunAddSecretRefusesMismatchedPassword(t *testing.T) {
	// The borrowed salt shares the directory's derivation, so
	// the password must open the secrets it came from: a
	// differing one is refused as wrong password before the
	// file is written. The --salt flag joins no derivation
	// group - the directory is unread under it - so its
	// passwords constrain nothing.
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(home, "secrets")
	addSecret(t, "api", zeroSalt, "pw", "tok")

	var stdout strings.Builder
	err := Run(context.Background(), RunOptions{
		Argv:   []string{"--add-secret", "beta"},
		Stdin:  strings.NewReader("other\ntok-beta\n"),
		Stdout: &stdout,
		Stderr: &stdout,
	})
	if err == nil || !strings.Contains(err.Error(), `wrong password: secret "api": cannot decrypt`) {
		t.Fatalf("error = %v, want wrong password naming api", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "beta.json")); !os.IsNotExist(err) {
		t.Errorf("beta.json: %v, want nothing written", err)
	}

	alt := make([]byte, 24)
	alt[0] = 1
	if err := Run(context.Background(), RunOptions{
		Argv:   []string{"--add-secret", "beta", "--salt", base64.StdEncoding.EncodeToString(alt)},
		Stdin:  strings.NewReader("other\ntok-beta\n"),
		Stdout: &stdout,
		Stderr: &stdout,
	}); err != nil {
		t.Fatalf("unexpected error under the flag: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "beta.json")); err != nil {
		t.Errorf("beta.json: %v, want the seal written", err)
	}
}

func TestRunAddSecretPrintsShortenedPath(t *testing.T) {
	// A file under the user's home confirms with the home
	// shortened to ~; the real location stays spellable.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("REBEAR_HOME", filepath.Join(home, "config", "rebear"))

	var stdout strings.Builder
	if err := Run(context.Background(), RunOptions{
		Argv:   []string{"--add-secret", "api"},
		Stdin:  strings.NewReader("pw\ntok\n"),
		Stdout: &stdout,
		Stderr: &stdout,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "secrets: wrote ~/config/rebear/secrets/api.json"
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRunAddSecretChecksNameFirst(t *testing.T) {
	// A bad name fails before anything is read from stdin.
	var stdout strings.Builder
	err := Run(context.Background(), RunOptions{
		Argv:   []string{"--add-secret", "Bad_Name"},
		Stdin:  strings.NewReader("pw\ntok\n"),
		Stdout: &stdout,
		Stderr: &stdout,
	})
	if err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("error = %v, want the name rule", err)
	}
}

func TestRunAddSecretSalt(t *testing.T) {
	// The flag's salt must decode to exactly the format's own
	// 24 bytes; a good one lands in the file verbatim, and a
	// directory without the flag draws a fresh salt.
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	dir := filepath.Join(home, "secrets")

	var stdout strings.Builder
	err := Run(context.Background(), RunOptions{
		Argv:   []string{"--add-secret", "api", "--salt", base64.StdEncoding.EncodeToString(make([]byte, 23))},
		Stdin:  strings.NewReader("pw\ntok\n"),
		Stdout: &stdout,
		Stderr: &stdout,
	})
	if err == nil || !strings.Contains(err.Error(), "standard base64 of 24 bytes") {
		t.Fatalf("error = %v, want the salt size", err)
	}

	salt := make([]byte, 24)
	salt[0] = 0xa5
	if err := Run(context.Background(), RunOptions{
		Argv:   []string{"--add-secret", "api", "--salt", base64.StdEncoding.EncodeToString(salt)},
		Stdin:  strings.NewReader("pw\ntok\n"),
		Stdout: &stdout,
		Stderr: &stdout,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := sealBody(t, filepath.Join(dir, "api.json"))["salt"]; got != base64.StdEncoding.EncodeToString(salt) {
		t.Errorf("salt = %q, want the flag's own", got)
	}
}

func TestRunAddSecretPipeEOF(t *testing.T) {
	// A bare EOF at either ask fails, naming the ask.
	var stdout strings.Builder
	err := Run(context.Background(), RunOptions{
		Argv:   []string{"--add-secret", "api"},
		Stdin:  strings.NewReader(""),
		Stdout: &stdout,
		Stderr: &stdout,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot read the secrets password") {
		t.Fatalf("error = %v, want the secrets password ask named", err)
	}
	err = Run(context.Background(), RunOptions{
		Argv:   []string{"--add-secret", "api"},
		Stdin:  strings.NewReader("pw\n"),
		Stdout: &stdout,
		Stderr: &stdout,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot read the secret") {
		t.Fatalf("error = %v, want the secret ask named", err)
	}
}

func TestRunStartupPassword(t *testing.T) {
	// A secrets directory asks for the password at startup and
	// reads the plaintexts into the snippet build; a pipe asks
	// plainly, and neither the password nor the plaintext rides
	// the log.
	remoteURL, server := remoteRecords(t, harness.Response{Status: 200, Body: "ok"})
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	addSecret(t, "api", zeroSalt, "pw", "tok-one")
	sock := filepath.Join(t.TempDir(), "ask.sock")
	path := writeSetupFile(t, secretCnf(sock, remoteURL, "api"))
	stdout, cancel, done := startReloadableIn(t, nil, path, strings.NewReader("pw\n"))
	waitFor(t, stdout, done, `"msg":"started listening"`)

	resp, err := harness.NewClient("unix:"+sock).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 || resp.Body != "ok" {
		t.Errorf("response = %d %q, want the outlet's", resp.Status, resp.Body)
	}
	last := server.LastRequest()
	if last == nil || last.Headers["Authorization"] != "Bearer tok-one" {
		t.Errorf("Authorization = %v, want the sealed token", last)
	}
	if strings.Contains(stdout.String(), "pw") {
		t.Errorf("stdout = %q, want no password", stdout.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunStartupWrongPassword(t *testing.T) {
	// The wrong password fails at startup, naming the first
	// secret the scan read, and nothing serves.
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	addSecret(t, "api", zeroSalt, "pw", "tok")
	path := writeSetupFile(t, setupContent(filepath.Join(t.TempDir(), "wrong.sock")))

	stdout, cancel, done := startReloadableIn(t, nil, path, strings.NewReader("wrong\n"))
	defer cancel()
	if err := <-done; err == nil || !strings.Contains(err.Error(), `secret "api": cannot decrypt`) {
		t.Errorf("error = %v, want api named", err)
	}
	if strings.Contains(stdout.String(), `"msg":"started listening"`) {
		t.Errorf("stdout = %q, want nothing served", stdout.String())
	}
}

func TestRunStartupScanFailure(t *testing.T) {
	// A secrets directory is the whole manifest: a stray entry
	// fails the startup, naming itself.
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	dir := filepath.Join(home, "secrets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("cannot create the secrets dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("cannot write the stray: %v", err)
	}
	path := writeSetupFile(t, setupContent(filepath.Join(t.TempDir(), "scan.sock")))

	_, cancel, done := startReloadableIn(t, nil, path, strings.NewReader(""))
	defer cancel()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "notes.txt") || !strings.Contains(err.Error(), "not a json file") {
		t.Errorf("error = %v, want the stray named", err)
	}
}

func TestRunSecretsReloadAddsFile(t *testing.T) {
	// SIGHUP re-reads the secrets directory: a file sealed under
	// the kept password serves once the new setup names it, and
	// no second ask happens - the password lives for the
	// process.
	remoteURL, server := remoteRecords(t, harness.Response{Status: 200, Body: "ok"})
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	secretsDir := filepath.Join(home, "secrets")
	addSecret(t, "alpha", zeroSalt, "pw", "tok-alpha")
	sock := filepath.Join(t.TempDir(), "sighup.sock")
	path := writeSetupFile(t, secretCnf(sock, remoteURL, "alpha"))
	stdout, cancel, done := startReloadableIn(t, nil, path, strings.NewReader("pw\n"))
	waitFor(t, stdout, done, `"msg":"started listening"`)

	// The added seal shares the directory's salt, so the reload
	// opens without a second derivation cost.
	salt, err := secrets.Salt(zeroSalt)
	if err != nil {
		t.Fatalf("cannot decode the salt: %v", err)
	}
	body, err := secrets.Encrypt("tok-beta", "pw", salt)
	if err != nil {
		t.Fatalf("cannot seal beta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "beta.json"), body, 0o600); err != nil {
		t.Fatalf("cannot write beta: %v", err)
	}
	writeSetupAt(t, path, secretCnf(sock, remoteURL, "beta"))
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("cannot signal: %v", err)
	}
	waitFor(t, stdout, done, `"msg":"SIGHUP - configuration reloaded"`)

	resp, err := harness.NewClient("unix:"+sock).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error after reload: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("response = %d, want the outlet's", resp.Status)
	}
	last := server.LastRequest()
	if last == nil || last.Headers["Authorization"] != "Bearer tok-beta" {
		t.Errorf("Authorization = %v, want the added secret", last)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// remoteRecords starts a harness server and returns its URL
// and the server itself, for asserting on what arrived.
func remoteRecords(t *testing.T, reply harness.Response) (string, *harness.Server) {
	t.Helper()
	server := harness.NewServer(reply)
	url, err := server.Start()
	if err != nil {
		t.Fatalf("cannot start upstream: %v", err)
	}
	t.Cleanup(func() { _ = server.Stop() })
	return url, server
}

func TestRunKeyringStartup(t *testing.T) {
	// --keyring takes the startup password from the keyring:
	// the lookup runs for the resolved home, stdin is never
	// touched - no prompt anywhere - and the sealed secret rides
	// a request.
	remoteURL, server := remoteRecords(t, harness.Response{Status: 200, Body: "ok"})
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	addSecret(t, "api", zeroSalt, "keyring-pw", "tok-one")
	args, _ := secretToolFake(t, map[string]string{home: "keyring-pw"})

	sock := filepath.Join(t.TempDir(), "tool.sock")
	path := writeSetupFile(t, secretCnf(sock, remoteURL, "api"))
	stdout, cancel, done := startReloadableIn(t, []string{"--keyring"}, path, strings.NewReader(""))
	waitFor(t, stdout, done, `"msg":"started listening"`)

	resp, err := harness.NewClient("unix:"+sock).Do("GET", "/", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 || resp.Body != "ok" {
		t.Errorf("response = %d %q, want the outlet's", resp.Status, resp.Body)
	}
	last := server.LastRequest()
	if last == nil || last.Headers["Authorization"] != "Bearer tok-one" {
		t.Errorf("Authorization = %v, want the sealed token", last)
	}
	if strings.Contains(stdout.String(), "secrets password") {
		t.Errorf("stdout = %q, want no password ask", stdout.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"lookup", "application", "rebear", "REBEAR_HOME", home}
	if lines := secretToolArgs(t, args); !slicesEqual(lines, want) {
		t.Errorf("lookup args = %q, want %q", lines, want)
	}
}

func TestRunKeyringLookupHome(t *testing.T) {
	// The lookup targets the home the run resolves, the same
	// resolution the config takes: the $REBEAR_HOME form under
	// the env var, else the XDG fallback. A wrong target is a
	// wrong password, so the listening line is the proof and
	// the recorded args spell the home.
	testCases := []struct {
		name string
		home func(t *testing.T) string
	}{{
		name: "env home",
		home: func(t *testing.T) string {
			home := t.TempDir()
			setEnv(t, "REBEAR_HOME", home)
			return home
		},
	}, {
		name: "xdg home",
		home: func(t *testing.T) string {
			home := t.TempDir()
			unsetEnv(t, "REBEAR_HOME")
			setEnv(t, "XDG_CONFIG_HOME", home)
			return filepath.Join(home, "rebear")
		},
	}}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			home := tc.home(t)
			addSecret(t, "api", zeroSalt, "pw", "tok-one")
			args, _ := secretToolFake(t, map[string]string{home: "pw"})

			sock := filepath.Join(t.TempDir(), "home.sock")
			path := writeSetupFile(t, setupContent(sock))
			stdout, cancel, done := startReloadableIn(t, []string{"--keyring"}, path, strings.NewReader(""))
			waitFor(t, stdout, done, `"msg":"started listening"`)
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			lines := secretToolArgs(t, args)
			if len(lines) != 5 || lines[0] != "lookup" || lines[4] != home {
				t.Errorf("lookup args = %q, want %q", lines, home)
			}
		})
	}
}

func TestRunKeyringTwoHomes(t *testing.T) {
	// The home is the entry's identity: two homes keep two
	// entries with two passwords, and each run opens only its
	// own - the other entry would fail the startup before any
	// listening.
	remoteURL, server := remoteRecords(t, harness.Response{Status: 200, Body: "ok"})
	testCases := []struct {
		name, home, password, plain string
	}{{
		name: "first", home: t.TempDir(), password: "pw-a", plain: "tok-a",
	}, {
		name: "second", home: t.TempDir(), password: "pw-b", plain: "tok-b",
	}}
	entries := map[string]string{}
	for _, tc := range testCases {
		entries[tc.home] = tc.password
	}
	secretToolFake(t, entries)
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, "REBEAR_HOME", tc.home)
			addSecret(t, "api", zeroSalt, tc.password, tc.plain)

			sock := filepath.Join(t.TempDir(), "two-homes.sock")
			path := writeSetupFile(t, secretCnf(sock, remoteURL, "api"))
			stdout, cancel, done := startReloadableIn(t, []string{"--keyring"}, path, strings.NewReader(""))
			waitFor(t, stdout, done, `"msg":"started listening"`)

			if _, err := harness.NewClient("unix:"+sock).Do("GET", "/", nil, ""); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			last := server.LastRequest()
			if last == nil || last.Headers["Authorization"] != "Bearer "+tc.plain {
				t.Errorf("Authorization = %v, want the home's own token", last)
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRunKeyringAddSecret(t *testing.T) {
	// --add-secret takes the flag too: the password comes from
	// the keyring, the secret still from stdin - one line - and
	// the borrowed-from secrets verify the keyring password:
	// the wrong entry fails with the usual line and writes
	// nothing.
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	addSecret(t, "api", zeroSalt, "pw", "tok-one")
	args, _ := secretToolFake(t, map[string]string{home: "pw"})

	var stdout strings.Builder
	if err := Run(context.Background(), RunOptions{
		Argv:   []string{"--add-secret", "beta", "--keyring"},
		Stdin:  strings.NewReader("tok-beta\n"),
		Stdout: &stdout,
		Stderr: &stdout,
	}); err != nil {
		t.Fatalf("cannot seal beta: %v", err)
	}
	sealed, err := secrets.Scan(filepath.Join(home, "secrets"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	opened, err := secrets.Open(sealed, "pw")
	if err != nil {
		t.Fatalf("cannot open: %v", err)
	}
	if opened["beta"] != "tok-beta" {
		t.Errorf("beta = %q, want the sealed plaintext", opened["beta"])
	}
	want := []string{"lookup", "application", "rebear", "REBEAR_HOME", home}
	if lines := secretToolArgs(t, args); !slicesEqual(lines, want) {
		t.Errorf("lookup args = %q, want %q", lines, want)
	}

	secretToolFake(t, map[string]string{home: "WRONG"})
	err = Run(context.Background(), RunOptions{
		Argv:   []string{"--add-secret", "gamma", "--keyring"},
		Stdin:  strings.NewReader("tok-gamma\n"),
		Stdout: &stdout,
		Stderr: &stdout,
	})
	if err == nil || !strings.Contains(err.Error(), `wrong password: secret "api": cannot decrypt`) {
		t.Errorf("error = %v, want wrong password naming api", err)
	}
	if _, err := os.Stat(filepath.Join(home, "secrets", "gamma.json")); !os.IsNotExist(err) {
		t.Errorf("gamma.json: %v, want nothing written", err)
	}
}

func TestRunKeyringAddSecretFresh(t *testing.T) {
	// A fresh seal under the flag has nothing to check the
	// keyring password against: it is taken as is, with no
	// second ask - the single stdin line stays for the secret,
	// where a second ask would starve it.
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	secretToolFake(t, map[string]string{home: "pw"})

	var stdout strings.Builder
	if err := Run(context.Background(), RunOptions{
		Argv:   []string{"--add-secret", "api", "--keyring"},
		Stdin:  strings.NewReader("tok-one\n"),
		Stdout: &stdout,
		Stderr: &stdout,
	}); err != nil {
		t.Fatalf("cannot seal: %v", err)
	}
	sealed, err := secrets.Scan(filepath.Join(home, "secrets"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	opened, err := secrets.Open(sealed, "pw")
	if err != nil {
		t.Fatalf("cannot open: %v", err)
	}
	if opened["api"] != "tok-one" {
		t.Errorf("api = %q, want tok-one", opened["api"])
	}
}

func TestRunKeyringMissingEntry(t *testing.T) {
	// A lookup that answers nothing - the real tool's missing
	// entry, exit 1 without complaint - fails the startup with
	// the refusal naming the home, and nothing serves.
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	addSecret(t, "api", zeroSalt, "pw", "tok-one")
	secretToolFake(t, map[string]string{})

	sock := filepath.Join(t.TempDir(), "missing.sock")
	path := writeSetupFile(t, setupContent(sock))
	stdout, cancel, done := startReloadableIn(t, []string{"--keyring"}, path, strings.NewReader(""))
	defer cancel()
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "--keyring: no rebear password for REBEAR_HOME") || !strings.Contains(err.Error(), fmt.Sprintf("%q", home)) {
		t.Errorf("error = %v, want the missing entry naming the home", err)
	}
	if strings.Contains(stdout.String(), `"msg":"started listening"`) {
		t.Errorf("stdout = %q, want nothing served", stdout.String())
	}
}

func TestRunKeyringFailedExec(t *testing.T) {
	// The tool's own failure embeds verbatim: its complaint
	// carries its name, and the refusal adds only the flag's.
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	addSecret(t, "api", zeroSalt, "pw", "tok-one")
	secretToolFailingFake(t, "secret-tool: Cannot spawn a message bus")

	sock := filepath.Join(t.TempDir(), "bus.sock")
	path := writeSetupFile(t, setupContent(sock))
	_, cancel, done := startReloadableIn(t, []string{"--keyring"}, path, strings.NewReader(""))
	defer cancel()
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "--keyring: secret-tool: Cannot spawn a message bus") {
		t.Errorf("error = %v, want the tool's complaint", err)
	}
}

func TestRunKeyringMissingBinary(t *testing.T) {
	// A missing binary fails with the tool named - LookPath's
	// own error - under the flag's prefix.
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	addSecret(t, "api", zeroSalt, "pw", "tok-one")
	t.Setenv("PATH", t.TempDir())

	sock := filepath.Join(t.TempDir(), "nobin.sock")
	path := writeSetupFile(t, setupContent(sock))
	_, cancel, done := startReloadableIn(t, []string{"--keyring"}, path, strings.NewReader(""))
	defer cancel()
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "--keyring:") || !strings.Contains(err.Error(), "not found in $PATH") {
		t.Errorf("error = %v, want the missing binary", err)
	}
}

func TestRunSetKeyringAgainstSecrets(t *testing.T) {
	// Seals on disk check the stored password: one ask on
	// stdin, the wrong password refused, and the store
	// carrying the same attributes a later --keyring reads -
	// the password raw, no newline, the documented spelling.
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	addSecret(t, "api", zeroSalt, "pw", "tok-one")
	args, stored := secretToolFake(t, map[string]string{})

	var stdout strings.Builder
	if err := Run(context.Background(), RunOptions{
		Argv:   []string{"--set-keyring"},
		Stdin:  strings.NewReader("pw\n"),
		Stdout: &stdout,
		Stderr: &stdout,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	saved, err := os.ReadFile(stored)
	if err != nil {
		t.Fatalf("cannot read the stored password: %v", err)
	}
	if string(saved) != "pw" {
		t.Errorf("stored = %q, want the password without its newline", saved)
	}
	want := []string{"store", "--label=rebear", "application", "rebear", "REBEAR_HOME", home}
	if lines := secretToolArgs(t, args); !slicesEqual(lines, want) {
		t.Errorf("store args = %q, want %q", lines, want)
	}
	if !strings.Contains(stdout.String(), "keyring: stored the password for ") {
		t.Errorf("stdout = %q, want the confirmation", stdout.String())
	}
}

func TestRunSetKeyringWrongPassword(t *testing.T) {
	// A password that opens nothing is never stored: the
	// refusal names the secret, and the fake tool never runs.
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	addSecret(t, "api", zeroSalt, "pw", "tok-one")
	args, stored := secretToolFake(t, map[string]string{})

	var stdout strings.Builder
	err := Run(context.Background(), RunOptions{
		Argv:   []string{"--set-keyring"},
		Stdin:  strings.NewReader("wrong\n"),
		Stdout: &stdout,
		Stderr: &stdout,
	})
	if err == nil || !strings.Contains(err.Error(), `wrong password: secret "api": cannot decrypt`) {
		t.Fatalf("error = %v, want the wrong password naming api", err)
	}
	if _, err := os.Stat(stored); !os.IsNotExist(err) {
		t.Errorf("stored = %v, want nothing written", err)
	}
	if _, err := os.Stat(args); !os.IsNotExist(err) {
		t.Errorf("the fake tool ran, want no invocation")
	}
}

func TestRunSetKeyringNoSecrets(t *testing.T) {
	// With no seals nothing can check the password: a terminal
	// asks it twice and a pipe once, and the store takes it.
	// An empty password is refused - a later lookup cannot tell
	// an empty entry from a missing one.
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	_, stored := secretToolFake(t, map[string]string{})

	var stdout strings.Builder
	if err := Run(context.Background(), RunOptions{
		Argv:   []string{"--set-keyring"},
		Stdin:  strings.NewReader("new-pw\n"),
		Stdout: &stdout,
		Stderr: &stdout,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	saved, err := os.ReadFile(stored)
	if err != nil {
		t.Fatalf("cannot read the stored password: %v", err)
	}
	if string(saved) != "new-pw" {
		t.Errorf("stored = %q, want the password", saved)
	}

	err = Run(context.Background(), RunOptions{
		Argv:   []string{"--set-keyring"},
		Stdin:  strings.NewReader("\n"),
		Stdout: &stdout,
		Stderr: &stdout,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot store an empty password") {
		t.Fatalf("error = %v, want the empty password refused", err)
	}
	saved, err = os.ReadFile(stored)
	if err != nil {
		t.Fatalf("cannot read the stored password: %v", err)
	}
	if string(saved) != "new-pw" {
		t.Errorf("stored = %q, want the first store standing", saved)
	}
}

func TestRunSetKeyringFailedExec(t *testing.T) {
	// The tool's own failure embeds verbatim, under the flag's
	// prefix - the store cannot even reach the keyring.
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
	secretToolFailingFake(t, "secret-tool: Cannot spawn a message bus")

	var stdout strings.Builder
	err := Run(context.Background(), RunOptions{
		Argv:   []string{"--set-keyring"},
		Stdin:  strings.NewReader("pw\n"),
		Stdout: &stdout,
		Stderr: &stdout,
	})
	if err == nil || !strings.Contains(err.Error(), "--set-keyring: secret-tool: Cannot spawn a message bus") {
		t.Fatalf("error = %v, want the tool's complaint", err)
	}
}

// slicesEqual reports whether two string slices agree.
func slicesEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
