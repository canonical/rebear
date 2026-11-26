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
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/canonical/rebear/internal/logging"
	"github.com/canonical/rebear/internal/notes"
	"github.com/canonical/rebear/internal/proxy"
	"github.com/canonical/rebear/internal/secrets"
	"github.com/canonical/rebear/internal/setup"
	"github.com/canonical/rebear/internal/tls"
)

// RunOptions carries the inputs for one run.
type RunOptions struct {
	// Argv is the argument list, without the program name.
	Argv []string
	// Stdout receives regular output; Stderr receives errors and help.
	Stdout io.Writer
	Stderr io.Writer
	// Stdin carries the asks: the password at startup when the
	// secrets directory holds anything, the password and the
	// secret under --add-secret. Nil means os.Stdin.
	Stdin io.Reader
	// SetupPath selects the configuration file; empty means
	// $REBEAR_HOME/rebear.cnf, then $XDG_CONFIG_HOME/rebear/rebear.cnf.
	SetupPath string
}

// Run parses the arguments, loads the setup, and serves every inlet
// until the context is cancelled or the process is interrupted. A
// SIGHUP reloads the configuration: the sockets are handed to the
// new generation, and the connections already flowing drain on the
// old one. The JSONL log stream goes to Stdout; Stderr stays for
// errors.
func Run(ctx context.Context, opts RunOptions) error {
	logging.SetWriter(opts.Stdout)
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}

	parsed, err := Parse(opts.Argv)
	if err != nil {
		var help *HelpRequested
		if errors.As(err, &help) {
			fmt.Fprint(opts.Stderr, Usage)
			return nil
		}
		return err
	}
	logging.SetIndent(parsed.Indent)

	setupPath := opts.SetupPath
	if setupPath == "" {
		if home := os.Getenv("REBEAR_HOME"); home != "" {
			setupPath = filepath.Join(home, "rebear.cnf")
		}
	}
	if setupPath == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("cannot resolve config directory: %v", err)
		}
		setupPath = filepath.Join(dir, "rebear", "rebear.cnf")
	}
	// The snapshots land beside the config, the same resolution
	// the config itself uses: $REBEAR_HOME/snapshots, else the
	// user config dir's rebear/snapshots. The notes, the tls
	// directory, and the secrets directory land the same way.
	var baseDir string
	if home := os.Getenv("REBEAR_HOME"); home != "" {
		baseDir = home
	} else if dir, err := os.UserConfigDir(); err == nil {
		baseDir = filepath.Join(dir, "rebear")
	}
	snapshotsDir := filepath.Join(baseDir, "snapshots")
	notesDir := filepath.Join(baseDir, "notes")
	tlsDir := filepath.Join(baseDir, "tls")
	secretsDir := filepath.Join(baseDir, "secrets")

	// The one-shot pair generation: the names come off the
	// command line, the pairs land in the tls directory, and
	// the process leaves without serving anything.
	if len(parsed.AddTLS) > 0 {
		store := tls.NewStore(tlsDir)
		for _, name := range parsed.AddTLS {
			if err := setup.CheckName(name); err != nil {
				return fmt.Errorf("--add-tls: %v", err)
			}
			if _, err := store.Generate(name, name); err != nil {
				return err
			}
			fmt.Fprintf(opts.Stdout, "tls: generated %s.{cert,key}\n", shortPath(filepath.Join(tlsDir, name)))
		}
		return nil
	}

	// The one-shot seal: the name comes off the command line,
	// the salt is picked, the password and the secret are read,
	// and the process leaves without serving anything.
	if parsed.AddSecret != "" {
		return sealSecret(opts, parsed, baseDir)
	}

	// The one-shot keyring store: the password is asked for,
	// checked, and stored in the keyring, and the process
	// leaves without serving anything.
	if parsed.SetKeyring {
		return setKeyring(opts, baseDir)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGHUP, syscall.SIGUSR1)
	defer signal.Stop(sigs)

	// The notes store is global and owned here: created once,
	// handed to every generation, flushed and closed at exit -
	// after the serving stops, so the last closings' changes
	// land in the day file.
	notesStore, err := notes.New(notesDir, time.Now())
	if err != nil {
		return err
	}
	defer func() {
		if err := notesStore.Flush(); err != nil {
			logging.Log("notes: cannot flush: " + err.Error())
		}
		notesStore.Close()
	}()

	var serving *proxy.Proxy
	defer func() {
		if serving != nil {
			_ = serving.Stop()
		}
	}()

	// The password lives for the process: the first ask fills it
	// in, every generation reuses it.
	var password string
	asked := false

	// Each pass serves one generation; a SIGHUP starts the next.
	var listeners map[string]net.Listener
	for {
		var p *proxy.Proxy
		var buildErr error
		// The secrets scan and open ride every generation, so a
		// file added or resealed lands at the next SIGHUP, under
		// the password the first ask read.
		var opened map[string]string
		sealed, err := secrets.Scan(secretsDir)
		if err != nil {
			buildErr = err
		} else if len(sealed) > 0 {
			if !asked {
				ask := newRunAsker(opts, parsed.Keyring, baseDir)
				password, err = ask.readPassword()
				if err != nil {
					return err
				}
				password, err = ask.verifyPassword(password, sealed)
				if err != nil {
					return err
				}
				asked = true
			}
			if opened, err = secrets.Open(sealed, password); err != nil {
				buildErr = err
			}
		}
		cfg, err := setup.Load(setupPath)
		if err != nil {
			cfg = nil
			if buildErr == nil {
				buildErr = fmt.Errorf("%s: %v", setupPath, err)
			}
		}
		if buildErr == nil {
			p, buildErr = proxy.New(&proxy.ProxyOptions{
				Setup:         cfg,
				LogAt:         parsed.LogAt,
				Listeners:     listeners,
				SnapshotsPath: snapshotsDir,
				Notes:         notesStore,
				TLSPath:       tlsDir,
				Secrets:       opened,
			})
		}
		if buildErr != nil {
			if listeners == nil {
				return buildErr
			}
			// A failed reload changes nothing: the current
			// setup keeps serving.
			logging.Log("SIGHUP - cannot reload configuration", "error", buildErr.Error())
		} else {
			reload := serving != nil
			if reload {
				serving.Retire()
			}
			serving = p
			listeners = p.Masters()
			p.Serve(ctx)
			if reload {
				logging.Log("SIGHUP - configuration reloaded")
			}
		}
		// The wait for the next pass. A USR1 stays on it - its
		// whole errand is the flush - while every other signal
		// flushes before its own work.
		reload := false
		for !reload {
			select {
			case <-ctx.Done():
				return nil
			case sig := <-sigs:
				if err := notesStore.Flush(); err != nil {
					logging.Log("notes: cannot flush: " + err.Error())
				}
				switch sig {
				case syscall.SIGUSR1:
				case syscall.SIGHUP:
					logging.Log("SIGHUP - reloading configuration")
					reload = true
				default:
					return nil
				}
			}
		}
	}
}

// newRunAsker builds the one asker of the run. --keyring
// points the password asks at the keyring entry the home
// names; without it, every ask reads stdin.
func newRunAsker(opts RunOptions, keyring bool, home string) *asker {
	keyringHome := ""
	if keyring {
		keyringHome = home
	}
	return newAsker(opts.Stdin, opts.Stderr, keyringHome)
}

// sealSecret is the --add-secret one-shot: the name is checked,
// the salt is picked, the password and the secret are read and
// confirmed, and the file is written. Overwriting an existing
// name is the rotation path, not an error.
func sealSecret(opts RunOptions, parsed *Options, baseDir string) error {
	if err := setup.CheckName(parsed.AddSecret); err != nil {
		return fmt.Errorf("--add-secret: %v", err)
	}
	dir := filepath.Join(baseDir, "secrets")
	salt, borrowed, err := pickSalt(parsed.Salt, dir)
	if err != nil {
		return err
	}
	ask := newRunAsker(opts, parsed.Keyring, baseDir)
	password, err := ask.readPassword()
	if err != nil {
		return err
	}
	password, err = ask.checkPassword(password, borrowed)
	if err != nil {
		return err
	}
	secret, err := ask.readSecret()
	if err != nil {
		return err
	}
	body, err := secrets.Encrypt(secret, password, salt)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, parsed.AddSecret+".json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(opts.Stdout, "secrets: wrote %s\n", shortPath(path))
	return nil
}

// setKeyring is the --set-keyring one-shot: the password
// is asked for on stdin and stored in the keyring entry the home
// names. The seals on disk check it first - one ask, the check
// in place of a second - and a password that opens nothing is
// refused; with no seals, a typed password is asked again
// until the two agree. The confirmed password is the stored
// one, and an empty one is refused: a later lookup cannot
// tell an empty entry from a missing one.
func setKeyring(opts RunOptions, home string) error {
	dir := filepath.Join(home, "secrets")
	sealed, err := secrets.Scan(dir)
	if err != nil {
		return err
	}
	ask := newRunAsker(opts, false, home)
	password, err := ask.readPassword()
	if err != nil {
		return err
	}
	// Whatever opens the seals on disk can start rebear, so
	// the stored entry must agree with every file.
	password, err = ask.checkPassword(password, sealed)
	if err != nil {
		return err
	}
	if password == "" {
		return fmt.Errorf("--set-keyring: cannot store an empty password")
	}
	if err := storeKeyringPassword(home, password); err != nil {
		return err
	}
	fmt.Fprintf(opts.Stdout, "keyring: stored the password for %s\n", shortPath(home))
	return nil
}

// pickSalt picks the seal's salt, reporting the secrets the
// salt was borrowed from: the flag's own base64, the
// directory's first secret's - in name order, the size every
// file carries - or a fresh draw on an empty directory. The
// flag and the draw borrow from nothing; a broken file fails
// the scan, naming the offender.
func pickSalt(flagged, dir string) ([]byte, []secrets.Secret, error) {
	if flagged != "" {
		salt, err := secrets.Salt(flagged)
		if err != nil {
			return nil, nil, fmt.Errorf("--salt: %v", err)
		}
		return salt, nil, nil
	}
	sealed, err := secrets.Scan(dir)
	if err != nil {
		return nil, nil, err
	}
	if len(sealed) > 0 {
		return sealed[0].Salt, sealed, nil
	}
	salt := make([]byte, 24)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, err
	}
	return salt, nil, nil
}

// shortPath names the file's real path, the user's home
// shortened to ~ when the path falls under it: the
// confirmation spells the file without the whole home tree.
func shortPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return abs
	}
	if abs == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(abs, home+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return abs
}
