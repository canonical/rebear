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

// Package snapshots dumps request and response payloads onto
// files on disk: one file per snapshot name per kind, teed at
// the delivery points, beside one aggregated log file per name.
package snapshots

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/canonical/rebear/internal/logging"
	"github.com/canonical/rebear/internal/plainjson"
	"github.com/canonical/rebear/internal/setup"
)

// CheckName reports whether name may snapshot, prefixing the
// refusal with the starlark call site's own name.
func CheckName(name string) error {
	if err := setup.CheckName(name); err != nil {
		return fmt.Errorf("snapshot: %v", err)
	}
	return nil
}

// File is one snapshot payload file: written as .partial, renamed
// on Commit. An uncommitted one keeps its .partial, the partial
// payload of a failed flow.
type File struct {
	// path is the final name; partial the .partial
	// form renamed onto it at Commit.
	path    string
	partial string
	file    *os.File
	// err carries the first failure: the later calls
	// no-op on it, never failing the flow.
	err error
}

// New opens the .partial file for one payload snapshot under dir.
// An empty dir is the feature off: nil, every later call a no-op.
func New(dir, name string, stamp time.Time, kind, mediaType string) *File {
	if dir == "" {
		return nil
	}
	ext := "bin"
	switch {
	case strings.Contains(mediaType, "json"):
		ext = "json"
	case strings.HasPrefix(mediaType, "text/"):
		ext = "txt"
	}
	f := &File{}
	f.path = filepath.Join(dir, fmt.Sprintf("%s-%s-%s.%s", name, logging.StampString(stamp), kind, ext))
	f.partial = f.path + ".partial"
	if f.ok(os.MkdirAll(dir, 0o755)) {
		file, err := os.Create(f.partial)
		if f.ok(err) {
			f.file = file
		}
	}
	return f
}

// Write never fails: a failed or unopened file swallows
// the bytes, the error already on record.
func (f *File) Write(p []byte) (int, error) {
	if f == nil || f.err != nil || f.file == nil {
		return len(p), nil
	}
	n, err := f.file.Write(p)
	if !f.ok(err) {
		return len(p), nil
	}
	return n, nil
}

// Commit drops the .partial suffix: the payload crossed whole.
// The first failure is what the call answers.
func (f *File) Commit() error {
	if f == nil {
		return nil
	}
	if f.file == nil {
		// Never opened: the failure is already on record.
		return f.err
	}
	if !f.ok(f.file.Close()) {
		// The rename would only pile a second error.
		f.file = nil
		return f.err
	}
	f.file = nil
	f.ok(os.Rename(f.partial, f.path))
	return f.err
}

// ok remembers the first error and reports whether this
// call succeeded.
func (f *File) ok(err error) bool {
	if err != nil && f.err == nil {
		f.err = err
	}
	return err == nil
}

// Abort drops the file without committing: the .partial stays,
// the partial payload of a flow that failed.
func (f *File) Abort() {
	if f == nil || f.file == nil {
		return
	}
	f.ok(f.file.Close())
	f.file = nil
}

// Log writes one name's log file: the flow's whole log list
// as one object, nothing when the flow logged nothing. The
// ordinary payload machinery under the log kind.
func Log(dir, name string, stamp time.Time, logs []map[string]any) error {
	if dir == "" || len(logs) == 0 {
		return nil
	}
	data, err := plainjson.Marshal(map[string]any{"log": logs})
	if err != nil {
		return err
	}
	// The trailing newline: the file is a text file, cat-friendly.
	file := New(dir, name, stamp, "log", "json")
	_, _ = file.Write(data)
	_, _ = file.Write([]byte("\n"))
	return file.Commit()
}
