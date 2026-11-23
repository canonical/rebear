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

package snapshots

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stamp is a fixed moment the filename assertions share.
var stamp = time.Date(2026, 11, 16, 12, 34, 56, 789000000, time.UTC)

// suffix is the stamp's filename form: the millisecond
// zero-padded to three digits.
const suffix = "20261116-123456-789"

func TestValidName(t *testing.T) {
	// The name lands in a filename: the sniplet name rule keeps
	// the path separators, the dots, and the case games out.
	testCases := []struct {
		name string
		ok   bool
	}{{
		name: "foo", ok: true,
	}, {
		name: "foo-bar", ok: true,
	}, {
		name: "a", ok: true,
	}, {
		name: "a-b-c2", ok: true,
	}, {
		name: "Foo", ok: false,
	}, {
		name: "foo_bar", ok: false,
	}, {
		name: "foo/bar", ok: false,
	}, {
		name: "../escape", ok: false,
	}, {
		name: "foo.bar", ok: false,
	}, {
		name: "-foo", ok: false,
	}, {
		name: "foo-", ok: false,
	}, {
		name: "", ok: false,
	}}
	for _, tc := range testCases {
		err := CheckName(tc.name)
		if tc.ok && err != nil {
			t.Errorf("CheckName(%q) = %v, want accepted", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("CheckName(%q) = nil, want refused", tc.name)
		}
	}
}

func TestFileGrammar(t *testing.T) {
	// The payload file names its snapshot, the stamp, the kind,
	// and the extension the content type picked: json anywhere,
	// text prefixed, bin otherwise. Committed files lose .partial.
	path := t.TempDir()
	dir := path
	testCases := []struct {
		mediaType string
		ext       string
	}{{
		mediaType: "application/json", ext: "json",
	}, {
		mediaType: "application/vnd.api+json", ext: "json",
	}, {
		mediaType: "text/event-stream", ext: "txt",
	}, {
		mediaType: "text/plain", ext: "txt",
	}, {
		mediaType: "application/octet-stream", ext: "bin",
	}, {
		mediaType: "", ext: "bin",
	}}
	for _, tc := range testCases {
		file := New(dir, "foo", stamp, "out", tc.mediaType)
		if _, err := file.Write([]byte("payload")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if err := file.Commit(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "foo-" + suffix + "-out." + tc.ext
		if !strings.HasSuffix(file.path, want) {
			t.Errorf("path = %q, want it to end in %q", file.path, want)
			continue
		}
		data, err := os.ReadFile(file.path)
		if err != nil {
			t.Fatalf("cannot read the committed file: %v", err)
		}
		if string(data) != "payload" {
			t.Errorf("content = %q, want payload", data)
		}
	}
}

func TestFileKeepsPartialWithoutCommit(t *testing.T) {
	// A file never committed keeps its .partial form: the partial
	// payload of a failed flow is the artifact.
	dir := t.TempDir()
	file := New(dir, "foo", stamp, "in", "application/json")
	if _, err := file.Write([]byte("half")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	file.Abort()
	if _, err := os.Stat(file.partial); err != nil {
		t.Errorf("the .partial file is gone: %v", err)
	}
	if _, err := os.Stat(file.path); err == nil {
		t.Errorf("the file committed itself")
	}
}

func TestFileEmptyPayload(t *testing.T) {
	// An empty payload commits an empty file: an empty body is a
	// body, and its snapshot is empty.
	dir := t.TempDir()
	file := New(dir, "empty", stamp, "out", "application/json")
	if err := file.Commit(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(file.path)
	if err != nil {
		t.Fatalf("cannot read the committed file: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("content = %q, want empty", data)
	}
}

func TestLog(t *testing.T) {
	// The log file holds the flow's log list under log:, one
	// file per name, committed at once in the plain marshal
	// form - an & lands raw, like the log line itself. A
	// flow that logged nothing writes no file at all.
	path := t.TempDir()
	dir := path
	if err := Log(dir, "foo", stamp, []map[string]any{{"one": 1}, {"two": "two"}, {"three": "a&b <c>"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(path, "foo-"+suffix+"-log.json"))
	if err != nil {
		t.Fatalf("cannot read the log file: %v", err)
	}
	if want := `{"log":[{"one":1},{"two":"two"},{"three":"a&b <c>"}]}` + "\n"; string(data) != want {
		t.Errorf("content = %q, want %q", data, want)
	}
	if err := Log(dir, "bar", stamp, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "bar-"+suffix+"-log.json")); !os.IsNotExist(err) {
		t.Errorf("empty log wrote a file, want none")
	}
}

func TestFileFailedOpenIsInert(t *testing.T) {
	// A file whose open failed - an unwritable directory under
	// a file path here - stays inert: Write swallows, Close and
	// Commit answer the recorded failure, nothing panics on
	// the file that never opened.
	dir := "/dev/null/nowhere"
	file := New(dir, "foo", stamp, "out", "application/json")
	if _, err := file.Write([]byte("payload")); err != nil {
		t.Fatalf("Write = %v, want nil even on a failed file", err)
	}
	file.Abort()
	err := file.Commit()
	if err == nil {
		t.Fatalf("Commit = nil, want the recorded open failure")
	}
}

func TestEmptyDirIsOff(t *testing.T) {
	// An empty dir answers nil files and no-op logs: the
	// feature disabled, the calls harmless.
	if file := New("", "foo", stamp, "out", "application/json"); file != nil {
		t.Errorf("New = %v, want nil", file)
	}
	if err := Log("", "foo", stamp, nil); err != nil {
		t.Errorf("Log = %v, want nil", err)
	}
}
