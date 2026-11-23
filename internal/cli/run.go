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
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/canonical/rebear/internal/logging"
	"github.com/canonical/rebear/internal/notes"
	"github.com/canonical/rebear/internal/proxy"
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
	// user config dir's rebear/snapshots. The notes and the tls
	// directory land the same way, under notes and tls.
	var baseDir string
	if home := os.Getenv("REBEAR_HOME"); home != "" {
		baseDir = home
	} else if dir, err := os.UserConfigDir(); err == nil {
		baseDir = filepath.Join(dir, "rebear")
	}
	snapshotsDir := filepath.Join(baseDir, "snapshots")
	notesDir := filepath.Join(baseDir, "notes")
	tlsDir := filepath.Join(baseDir, "tls")

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
			fmt.Fprintf(opts.Stdout, "tls: generated $REBEAR_HOME/tls/%s.{cert,key}\n", name)
		}
		return nil
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

	// Each pass serves one generation; a SIGHUP starts the next.
	var listeners map[string]net.Listener
	for {
		var p *proxy.Proxy
		var buildErr error
		cfg, err := setup.Load(setupPath)
		if err == nil {
			p, buildErr = proxy.New(&proxy.ProxyOptions{
				Setup:         cfg,
				LogAt:         parsed.LogAt,
				Listeners:     listeners,
				SnapshotsPath: snapshotsDir,
				Notes:         notesStore,
				TLSPath:       tlsDir,
			})
		} else {
			buildErr = fmt.Errorf("%s: %v", setupPath, err)
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
