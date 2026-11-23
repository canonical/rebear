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

// Package notes holds the global notes map shared by every
// flow: one JSON string as the canonical state, handed out
// and taken back under the store lock, and flushed onto one
// day file per day under the notes directory.
package notes

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/canonical/rebear/internal/logging"
)

// flushEvery is the periodic flush interval.
const flushEvery = 60 * time.Second

// timeNow is the wall clock the flush writes under; tests may
// swap it. Day rollover rides on it.
var timeNow = time.Now

// Store is the global notes state, shared by every flow and
// handed over across reloads. The canonical form is one JSON
// string: Acquire hands it out under the lock, Release takes
// it back, so concurrent users serialize and the state only
// ever changes as a whole. The flush writes the string onto
// the day file whenever it changed since the last write.
type Store struct {
	mu sync.Mutex
	// path is the notes directory; empty is the feature off -
	// the state lives in memory, nothing loads or flushes.
	path string
	// state is the canonical JSON text of the map.
	state string
	// flushed is the text as last written; a state differing
	// from it is the change waiting for the flush.
	flushed string
	// notBefore is the day the written file must not be older
	// than: a day file from the future was loaded, so a flush
	// must not slide the state onto an older name that a later
	// start would then ignore.
	notBefore time.Time
	// stop ends the periodic flusher.
	stop chan struct{}
}

// New loads the notes state from dir: the latest day file is the
// current state, the files being cumulative views of the whole
// map rather than the intra-day changes. A file up to one day
// into the future is used - clock shifts stay survivable - and
// the flush then cannot write a file older than it; any further
// ahead fails: the clock is wrong, and pretending otherwise
// would strand the state. An empty dir is the feature off.
func New(dir string, now time.Time) (*Store, error) {
	s := &Store{path: dir, state: "{}", stop: make(chan struct{})}
	if dir != "" {
		if err := s.load(now); err != nil {
			return nil, err
		}
	}
	s.flushed = s.state
	if dir != "" {
		go s.loop()
	}
	return s, nil
}

// load reads the latest day file into the state.
func (s *Store) load(now time.Time) error {
	entries, err := os.ReadDir(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// The parse is the whole name check: a day-shaped name
	// that is not a day is not a notes file, not ours to fail
	// on. ReadDir answers sorted, so the last parseable name
	// is the latest day.
	var latest string
	var day time.Time
	for i := len(entries) - 1; i >= 0; i-- {
		name := entries[i].Name()
		d, err := time.Parse("20060102.json", name)
		if err != nil {
			continue
		}
		latest, day = name, d
		break
	}
	if latest == "" {
		return nil
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if day.After(today.AddDate(0, 0, 1)) {
		return fmt.Errorf("notes file is in the future: %s", filepath.Join(s.path, latest))
	}
	if day.After(today) {
		s.notBefore = day
	}
	data, err := os.ReadFile(filepath.Join(s.path, latest))
	if err != nil {
		return err
	}
	s.state = strings.TrimRight(string(data), "\n")
	return nil
}

// Acquire hands the canonical JSON string over and holds
// the store: until the matching Release, no other acquire
// goes through. The caller owns the string - its changes
// land through Release alone.
func (s *Store) Acquire() string {
	s.mu.Lock()
	return s.state
}

// Release takes the canonical JSON string back and opens the
// store. The string is not checked: the caller decides what
// the state is, the store only guarantees the custody.
func (s *Store) Release(state string) {
	s.state = state
	s.mu.Unlock()
}

// Flush writes the state onto the day file when it changed since
// the last write, atomically: a .partial file renamed into place.
// The day is today's, or the loaded future day when that is
// further - the state must never slide onto an older file.
func (s *Store) Flush() error {
	if s == nil || s.path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == s.flushed {
		return nil
	}
	day := timeNow()
	if s.notBefore.After(day) {
		day = s.notBefore
	}
	path := filepath.Join(s.path, day.Format("20060102")+".json")
	if err := os.MkdirAll(s.path, 0o755); err != nil {
		return err
	}
	partial := path + ".partial"
	file, err := os.Create(partial)
	if err != nil {
		return err
	}
	// The newline is its own write: the state string
	// is never re-allocated.
	_, err = file.WriteString(s.state)
	if err == nil {
		_, err = file.WriteString("\n")
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(partial, path); err != nil {
		return err
	}
	s.flushed = s.state
	return nil
}

// loop flushes the notes periodically, when they changed.
func (s *Store) loop() {
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			if err := s.Flush(); err != nil {
				logging.Log("notes: cannot flush: " + err.Error())
			}
		}
	}
}

// Close stops the periodic flusher. The store stays usable.
func (s *Store) Close() {
	close(s.stop)
}
