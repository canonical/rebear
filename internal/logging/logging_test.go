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

package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// stampRule matches the stamp form the stream's times carry:
// YYYYMMDD-HHMMSS-mss, the snapshot filenames' own.
var stampRule = regexp.MustCompile(`^\d{8}-\d{6}-\d{3}$`)

// setWriter swaps the stream sink for the test's duration and
// returns the buffer collecting the output.
func setWriter(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	SetWriter(buf)
	t.Cleanup(func() { SetWriter(nil) })
	return buf
}

// lines splits the buffer into its JSONL lines, dropping the
// trailing empty one.
func lines(buf *bytes.Buffer) []string {
	return strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
}

// decode unmarshals one line, failing the test on bad JSON.
func decode(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("cannot decode line %q: %v", line, err)
	}
	return m
}

func TestWriteKeyOrder(t *testing.T) {
	// The keys come out in the struct's order: method and
	// path right after time - so the start record and the
	// final one align - then the out-path beside it when the
	// URL was rewritten, the route, the outgoing request's
	// timings first (it leaves before the response comes
	// back), the response's code, the incoming response's
	// timings, the error last.
	buf := setWriter(t)
	Write(Entry{
		Time:    Time{time.Now()},
		Method:  "POST",
		Path:    "/v1/foo",
		OutPath: "/api/foo",
		Flow:    []string{"inlet", "outlet"},
		OutMs:   1,
		OutSize: 2,
		Code:    404,
		InMs:    3,
		InSize:  4,
		Error:   "boom",
	})
	want := []string{
		"time", "method", "path", "out-path", "flow", "out-ms",
		"out-size", "code", "in-ms", "in-size", "error",
	}
	last := -1
	for _, key := range want {
		next := strings.Index(buf.String(), `"`+key+`":`)
		if next < 0 {
			t.Fatalf("line %q has no key %q", buf.String(), key)
		}
		if next < last {
			t.Errorf("key %q out of order in %q", key, buf.String())
		}
		last = next
	}
}

func TestWriteOmitEmpty(t *testing.T) {
	// Every key but time and flow is dropped when empty: a
	// record holding nothing else shrinks to the pair.
	buf := setWriter(t)
	Write(Entry{Time: Time{time.Now()}, Flow: []string{"local"}})
	m := decode(t, lines(buf)[0])
	if len(m) != 2 {
		t.Errorf("line = %q, want only time and flow", m)
	}
	if _, ok := m["time"]; !ok {
		t.Errorf("line = %q, want a time key", m)
	}
	if !reflect.DeepEqual(m["flow"], []any{"local"}) {
		t.Errorf("flow = %v, want [local]", m["flow"])
	}
}

func TestWritePlainStrings(t *testing.T) {
	// The strings marshal plain: an &, <, or > rides the
	// line raw - no Go html escape, so the query holds the
	// wire form - and the newline still escapes, keeping the
	// record one line.
	buf := setWriter(t)
	e := Begin("GET", "/x", "poll=1&since=fuFmZm33sr")
	e.Error = "bad <tag>\nand more"
	Write(*e)
	written := lines(buf)
	if len(written) != 1 {
		t.Fatalf("output = %q, want one line", buf.String())
	}
	if !strings.Contains(written[0], `"query":"poll=1&since=fuFmZm33sr"`) {
		t.Errorf("line = %q, want the raw ampersand in the query", written[0])
	}
	if !strings.Contains(written[0], `"error":"bad <tag>\nand more"`) {
		t.Errorf("line = %q, want the plain tag and the escaped newline", written[0])
	}
}

func TestWriteFillsTime(t *testing.T) {
	// A zero Time is filled at write time, so the log lines
	// need no clock of their own. The form is the stamp form,
	// the snapshot filenames' own: a log line's time names its
	// snapshot family exactly.
	buf := setWriter(t)
	Log("started listening", "inlet", "local", "address", "http://127.0.0.1:1")
	m := decode(t, lines(buf)[0])
	s, ok := m["time"].(string)
	if !ok {
		t.Fatalf("line = %q, want a time key", m)
	}
	if !stampRule.MatchString(s) {
		t.Errorf("time %q, want the YYYYMMDD-HHMMSS-mss stamp form", s)
	}
}

func TestBeginSentDone(t *testing.T) {
	// The markers turn the recorded moments into the two
	// spans and sizes: out from the start to the send, in
	// from the send to the write-back. The stamps start
	// fresh: earlier entries in the same process may have
	// ticked the stamp ahead of the wall clock.
	resetStamps()
	e := Begin("POST", "/x", "")
	e.start = e.start.Add(-90 * time.Millisecond)
	e.Sent(11)
	e.sent = e.sent.Add(-50 * time.Millisecond)
	e.Done(22)
	if e.OutMs < 90 || e.OutMs > 95 {
		t.Errorf("OutMs = %d, want around 90", e.OutMs)
	}
	if e.OutSize != 11 {
		t.Errorf("OutSize = %d, want 11", e.OutSize)
	}
	if e.InMs < 50 || e.InMs > 55 {
		t.Errorf("InMs = %d, want around 50", e.InMs)
	}
	if e.InSize != 22 {
		t.Errorf("InSize = %d, want 22", e.InSize)
	}
	buf := setWriter(t)
	Write(*e)
	m := decode(t, lines(buf)[0])
	if m["method"] != "POST" || m["path"] != "/x" {
		t.Errorf("line = %q, want the method and path", m)
	}
}

func TestSubMillisecondSpansRoundUp(t *testing.T) {
	// A span under 1 ms rounds up to 1, not down to 0: a send
	// that happened must stay visible despite omitempty. The
	// stamps start fresh, so the send's time.Now() lands
	// after the entry's stamped start.
	resetStamps()
	e := Begin("POST", "/x", "")
	e.start = e.Time.Time
	e.sent = e.Time.Time.Add(time.Microsecond)
	e.Sent(11)
	e.sent = e.Time.Time.Add(time.Microsecond)
	e.Done(22)
	if e.OutMs != 1 {
		t.Errorf("OutMs = %d, want 1", e.OutMs)
	}
	if e.InMs != 1 {
		t.Errorf("InMs = %d, want 1", e.InMs)
	}
	buf := setWriter(t)
	Write(*e)
	m := decode(t, lines(buf)[0])
	if m["out-ms"] != float64(1) || m["in-ms"] != float64(1) {
		t.Errorf("line = %q, want both millisecond keys present", m)
	}
}

func TestStampNeverRepeats(t *testing.T) {
	// Stamp answers strictly increasing times, on whole
	// millisecond boundaries: calls within one millisecond -
	// the common case, with tests or busy requests stacked
	// up - tick forward instead, and a wall clock stepping
	// back cannot go back either.
	first := Stamp()
	for i := 0; i < 100; i++ {
		next := Stamp()
		if !next.After(first) {
			t.Fatalf("Stamp went back or stood still: %v then %v", first, next)
		}
		if next.Sub(first) != next.Sub(first).Truncate(time.Millisecond) {
			t.Fatalf("Stamp %v is off the millisecond boundary", next)
		}
		first = next
	}
}

func TestSetOutPath(t *testing.T) {
	// The client's path is the plain path from the start;
	// out-path joins only when the outlet received a different
	// one.
	e := Begin("GET", "/v1/foo", "")
	if e.Path != "/v1/foo" {
		t.Errorf("Path = %q, want the client's path", e.Path)
	}
	e.SetOutPath("/v1/foo")
	if e.OutPath != "" {
		t.Errorf("OutPath = %q, want it dropped on a match", e.OutPath)
	}
	e.SetOutPath("/api/foo")
	if e.OutPath != "/api/foo" {
		t.Errorf("OutPath = %q, want the diverging path", e.OutPath)
	}
}

func TestSetOutQuery(t *testing.T) {
	// The client's query rides the start record; out-query
	// joins only when the outlet received a different one.
	// Write caps both at 64 characters - cut at 61 with a ...
	// tail beyond - on its own copy: the record keeps the
	// exact forms the divergence check needs.
	e := Begin("GET", "/v1/foo", "a=1&b=2")
	if e.Query != "a=1&b=2" {
		t.Errorf("Query = %q, want the client's query", e.Query)
	}
	e.SetOutQuery("a=1&b=2")
	if e.OutQuery != "" {
		t.Errorf("OutQuery = %q, want it dropped on a match", e.OutQuery)
	}
	e.SetOutQuery("a=3")
	if e.OutQuery != "a=3" {
		t.Errorf("OutQuery = %q, want the diverging query", e.OutQuery)
	}

	// Two long and equal queries record no divergence, and the
	// written line caps both.
	long := strings.Repeat("x", 70)
	e = Begin("GET", "/", long)
	e.SetOutQuery(long)
	if e.OutQuery != "" {
		t.Errorf("OutQuery = %q, want it dropped on the match", e.OutQuery)
	}
	e.SetOutQuery(long + "&y=1")
	buf := setWriter(t)
	Write(*e)
	m := decode(t, lines(buf)[0])
	want := strings.Repeat("x", 61) + "..."
	if m["query"] != want {
		t.Errorf("query = %q, want %q", m["query"], want)
	}
	if wantOut := strings.Repeat("x", 61) + "..."; m["out-query"] != wantOut {
		t.Errorf("out-query = %q, want %q", m["out-query"], wantOut)
	}
}

func TestLog(t *testing.T) {
	// Log appends one object per call, in order, under the
	// record's log key; an entry without calls omits the key.
	e := Begin("GET", "/", "")
	e.Log(map[string]any{"key": "value"})
	e.Log(map[string]any{"other": 1})
	buf := setWriter(t)
	Write(*e)
	m := decode(t, lines(buf)[0])
	want := []any{
		map[string]any{"key": "value"},
		map[string]any{"other": float64(1)},
	}
	if !reflect.DeepEqual(m["log"], want) {
		t.Errorf("log = %q, want %q", m["log"], want)
	}

	bare := Begin("GET", "/", "")
	buf2 := setWriter(t)
	Write(*bare)
	if _, ok := decode(t, lines(buf2)[0])["log"]; ok {
		t.Errorf("line = %q, want no log key without calls", lines(buf2)[0])
	}
}

func TestLogStray(t *testing.T) {
	// A message from outside any request rides a bare log
	// list on a line of its own: msg plus the key/value pairs
	// beside it. Callers format their own message.
	buf := setWriter(t)
	Log(fmt.Sprintf("reload: %s took %dms", "setup", 42), "pid", 1234)
	m := decode(t, lines(buf)[0])
	want := []any{map[string]any{"msg": "reload: setup took 42ms", "pid": float64(1234)}}
	if len(m) != 2 || !reflect.DeepEqual(m["log"], want) {
		t.Errorf("line = %q, want only time and %q", m, want)
	}

	// An empty msg drops the key: the pairs stand alone. A
	// dangling key with no value is dropped too.
	buf = setWriter(t)
	Log("", "event", "reload", "orphan")
	m = decode(t, lines(buf)[0])
	want = []any{map[string]any{"event": "reload"}}
	if len(m) != 2 || !reflect.DeepEqual(m["log"], want) {
		t.Errorf("line = %q, want only time and %q", m, want)
	}
}

func TestSetIndent(t *testing.T) {
	// SetIndent expands each record into a multi-line object;
	// an empty indent restores the one-line form. The record
	// separator stays the single newline either way.
	setIndent := func(pad string) *bytes.Buffer {
		t.Helper()
		buf := setWriter(t)
		SetIndent(pad)
		t.Cleanup(func() { SetIndent("") })
		return buf
	}
	buf := setIndent("  ")
	Log("reload")
	Log("again")
	got := buf.String()
	// Two records, each a multi-line object opening with time
	// and carrying the indented log list.
	records := strings.Split(got, "}\n{")
	if len(records) != 2 {
		t.Fatalf("output = %q, want two records", got)
	}
	for i, msg := range []string{"reload", "again"} {
		// The separator ate a brace on each side; restore it.
		record := records[i]
		if i > 0 {
			record = "{" + record
		}
		record = strings.TrimSuffix(record, "\n")
		if i < len(records)-1 {
			record += "}"
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(record), &m); err != nil {
			t.Fatalf("record %d is not valid JSON: %v", i, err)
		}
		if _, ok := m["time"]; !ok {
			t.Errorf("record %d = %q, want a time key", i, m)
		}
		if !strings.Contains(got, "  \"log\": [\n    {\n      \"msg\": \""+msg+"\"\n    }\n  ]") {
			t.Errorf("output = %q, want the indented log list for %q", got, msg)
		}
	}

	buf = setIndent("")
	Log("one line")
	if strings.Contains(buf.String(), "\n ") {
		t.Errorf("output = %q, want the one-line form", buf.String())
	}
	one := decode(t, strings.Split(strings.TrimSpace(buf.String()), "\n")[0])
	if len(one) != 2 || !reflect.DeepEqual(one["log"], []any{map[string]any{"msg": "one line"}}) {
		t.Errorf("line = %q, want only time and the log list", one)
	}
}

func TestWriteNoWriter(t *testing.T) {
	// No writer set: the write is a no-op, not a panic.
	SetWriter(nil)
	Log("started listening", "inlet", "local", "address", "/sock")
}
