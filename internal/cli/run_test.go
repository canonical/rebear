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
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/canonical/rebear/internal/harness"
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
	// and exits without serving; an existing name or a bad one
	// is refused, and nothing else is touched.
	home := t.TempDir()
	t.Setenv("REBEAR_HOME", home)
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
	if !strings.Contains(stdout.String(), "tls: generated $REBEAR_HOME/tls/foo.{cert,key}") {
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
