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

package notes

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newStore builds a store over a temp dir, closing it at the
// test's end. The now fixes the day the store starts on.
func newStore(t *testing.T, now time.Time) *Store {
	t.Helper()
	s, err := New(t.TempDir(), now)
	if err != nil {
		t.Fatalf("cannot create store: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// writeDay writes one day file into dir.
func writeDay(t *testing.T, dir, day, state string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("cannot create dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, day+".json"), []byte(state), 0o644); err != nil {
		t.Fatalf("cannot write day file: %v", err)
	}
}

func TestNewLoadsLatestDayFile(t *testing.T) {
	// The latest day file is the current state: the files are
	// cumulative, so an older one carries nothing new. The
	// trailing newline the flush writes is not state.
	dir := t.TempDir()
	writeDay(t, dir, "20261114", `{"a":1}`)
	writeDay(t, dir, "20261115", "{\"b\":2}\n")
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	s, err := New(dir, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(s.Close)
	if s.state != `{"b":2}` {
		t.Errorf("state = %q, want the latest day file", s.state)
	}
	if s.flushed != `{"b":2}` {
		t.Errorf("flushed = %q, want the state: a newline on disk must not read as a change", s.flushed)
	}
}

func TestNewFutureDayFile(t *testing.T) {
	// A day file up to one day ahead is used, and the flush
	// cannot then write a file older than it - the state must
	// not slide onto a name a later start would ignore.
	dir := t.TempDir()
	writeDay(t, dir, "20261117", `{"x":1}`)
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	defer FakeTime(func() time.Time { return now })()
	s, err := New(dir, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(s.Close)
	s.Acquire()
	s.Release(`{"x":1,"y":2}`)
	if err := s.Flush(); err != nil {
		t.Fatalf("cannot flush: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "20261117.json"))
	if err != nil {
		t.Fatalf("cannot read day file: %v", err)
	}
	if string(data) != `{"x":1,"y":2}`+"\n" {
		t.Errorf("day file = %q, want the flushed state", data)
	}
}

func TestNewFarFutureDayFileFails(t *testing.T) {
	// A day file more than one day ahead is a clock gone
	// wrong: using it would strand the state behind a day no
	// flush can reach.
	dir := t.TempDir()
	writeDay(t, dir, "20261119", `{"x":1}`)
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	_, err := New(dir, now)
	path := filepath.Join(dir, "20261119.json")
	if err == nil || err.Error() != "notes file is in the future: "+path {
		t.Errorf("error = %v, want the future failure on %s", err, path)
	}
}

func TestAcquireReleaseCustody(t *testing.T) {
	// Acquire answers the state and holds the store; Release
	// takes the next state back and opens it - the strings
	// travel whole, the store never looking inside.
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	s := newStore(t, now)
	s.state = `{"a":1}`
	if state := s.Acquire(); state != `{"a":1}` {
		t.Errorf("Acquire() = %q, want the state", state)
	}
	s.Release(`{"a":1,"b":2}`)
	if s.state != `{"a":1,"b":2}` {
		t.Errorf("state = %q, want the released string", s.state)
	}
	if state := s.Acquire(); state != `{"a":1,"b":2}` {
		t.Errorf("Acquire() = %q, want the released state", state)
	}
	s.Release(`{"a":1,"b":2}`)
}

func TestFlushOnlyWhenChanged(t *testing.T) {
	// The flush writes the day file when the state changed,
	// and skips the write when it did not.
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	defer FakeTime(func() time.Time { return now })()
	s := newStore(t, now)
	s.state = `{"a":1}`
	if err := s.Flush(); err != nil {
		t.Fatalf("cannot flush: %v", err)
	}
	day := now.Format("20060102") + ".json"
	path := filepath.Join(s.path, day)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read day file: %v", err)
	}
	if string(data) != `{"a":1}`+"\n" {
		t.Errorf("day file = %q, want the state", data)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("cannot remove day file: %v", err)
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("cannot flush: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("day file = %v, want no rewrite without a change", err)
	}
}

func TestLockSerializesAccess(t *testing.T) {
	// The lock is the whole concurrency story: held, nobody
	// else acquires; released, the next one goes through.
	s := newStore(t, time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC))
	s.Acquire()
	acquired := make(chan struct{})
	go func() {
		s.Acquire()
		close(acquired)
		s.Release("")
	}()
	select {
	case <-acquired:
		t.Fatalf("store acquired while held")
	case <-time.After(50 * time.Millisecond):
	}
	s.Release("")
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatalf("store not released")
	}
}

func TestFlushRollover(t *testing.T) {
	// A fake clock may still move: the next day's first flush
	// writes the full state under that day's name.
	day := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	defer FakeTime(func() time.Time { return day })()
	s := newStore(t, day)
	s.Acquire()
	s.Release(`{"a":1}`)
	if err := s.Flush(); err != nil {
		t.Fatalf("cannot flush: %v", err)
	}
	day = day.AddDate(0, 0, 1)
	s.Acquire()
	s.Release(`{"a":1,"b":2}`)
	if err := s.Flush(); err != nil {
		t.Fatalf("cannot flush: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(s.path, "20261117.json"))
	if err != nil {
		t.Fatalf("cannot read new day file: %v", err)
	}
	if string(data) != `{"a":1,"b":2}`+"\n" {
		t.Errorf("new day file = %q, want the full state", data)
	}
}
