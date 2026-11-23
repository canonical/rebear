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

// Package logging writes rebear's log as one JSON object per
// line: request records and the startup listening reports.
// Stray messages from outside any request ride a bare log
// list on a line of their own.
package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/canonical/rebear/internal/plainjson"
)

var (
	mu     sync.Mutex
	writer io.Writer
	indent string
	// last is Stamp's previous answer, kept under mu so the
	// stamps order with the stream itself.
	last time.Time
)

// Stamp returns the current time, never the same answer twice: a
// call landing within the previous stamp's millisecond - or before
// it, on a wall clock stepping back - answers the next millisecond
// instead. One progression for the log times and the snapshot
// filenames, so the two align.
func Stamp() time.Time {
	mu.Lock()
	defer mu.Unlock()
	now := time.Now().Truncate(time.Millisecond)
	if !last.IsZero() && !now.After(last) {
		now = last.Add(time.Millisecond)
	}
	last = now
	return now
}

// resetStamps drops the accumulated lead: a test battery that
// begins many entries within one millisecond ticks the stamp
// ahead of the wall clock, breaking later time.Now() comparisons.
func resetStamps() {
	mu.Lock()
	defer mu.Unlock()
	last = time.Time{}
}

// SetWriter sets the sink the JSONL stream goes to. A nil writer
// drops the output.
func SetWriter(w io.Writer) {
	mu.Lock()
	writer = w
	mu.Unlock()
}

// SetIndent expands every record into a multi-line object
// with the given per-level padding, as json.Indent does;
// empty restores the one-line form. Call it before the
// stream starts.
func SetIndent(prefix string) {
	mu.Lock()
	indent = prefix
	mu.Unlock()
}

// StampString renders t as the stamp form every time a line or
// a filename carries: YYYYMMDD-HHMMSS-mss. One form, so a log
// line's time names its snapshot family exactly.
func StampString(t time.Time) string {
	return fmt.Sprintf("%04d%02d%02d-%02d%02d%02d-%03d",
		t.Year(), t.Month(), t.Day(),
		t.Hour(), t.Minute(), t.Second(),
		t.Nanosecond()/int(time.Millisecond))
}

// Time is a time.Time that marshals in the stamp form.
type Time struct{ time.Time }

func (t Time) MarshalJSON() ([]byte, error) {
	return []byte(`"` + StampString(t.Time) + `"`), nil
}

// Entry is one JSONL line. The field order is the JSON key
// order: method and path right after time, so the start
// record and the final one align; then the rewritten path,
// the route, the tls-auth name, the timings, the error, and
// the log list last.
// The startup line reuses the entry with its own keys; a stray
// message rides the log list on a bare entry.
type Entry struct {
	// Time is when the record's subject started.
	Time Time `json:"time"`

	// Method is the client's method; OutMethod joins it only
	// when the outlet received a different one.
	Method    string `json:"method,omitempty"`
	OutMethod string `json:"out-method,omitempty"`

	// Path is the client's path; OutPath joins it only when
	// the outlet received a different one. Both are outgoing
	// really - the plain name keeps the common case short.
	Path    string `json:"path,omitempty"`
	OutPath string `json:"out-path,omitempty"`

	// Query is the client's query and OutQuery the outlet's,
	// joined only on divergence. Write caps both at 64
	// characters, cut at 61 with a ... tail beyond.
	Query    string `json:"query,omitempty"`
	OutQuery string `json:"out-query,omitempty"`

	// Flow holds the inlet, midlets, outlet handling the request.
	Flow []string `json:"flow,omitempty"`

	// TLSAuth is the tls-auth name the client's certificate
	// pinned to, empty when the inlet pins nothing or the
	// client matched no pin.
	TLSAuth string `json:"tls-auth,omitempty"`

	// OutMs and OutSize are the timing and size of request going out.
	OutMs   int64 `json:"out-ms,omitempty"`
	OutSize int64 `json:"out-size,omitempty"`

	// Code holds the status code the client saw; InCode the
	// outlet's, joined only when the two diverge.
	Code   int64 `json:"code,omitempty"`
	InCode int64 `json:"in-code,omitempty"`

	// InMs and InSize are the timing and size of response coming in.
	InMs   int64 `json:"in-ms,omitempty"`
	InSize int64 `json:"in-size,omitempty"`

	Error string `json:"error,omitempty"`

	// Logs holds one object per log() call, appended in call
	// order. The json key is the singular log.
	Logs []map[string]any `json:"log,omitempty"`

	// start and sent time the two spans; the markers turn
	// them into the ints above.
	start time.Time
	sent  time.Time
}

// Begin opens one request record: the client's method, path,
// and query, and the start both spans count from. The time is a
// Stamp, so the snapshot filenames the request may produce align
// with it.
func Begin(method, path, query string) *Entry {
	now := Stamp()
	return &Entry{Time: Time{now}, start: now, Method: method, Path: path, Query: query}
}

// Sent stamps the outlet send: the outgoing request's span
// so far and its body size. The span rounds up to whole
// milliseconds, so a fast send still shows as 1 rather than
// vanishing under omitempty.
func (e *Entry) Sent(size int64) {
	e.sent = time.Now()
	e.OutMs = ceilMs(e.sent.Sub(e.start))
	e.OutSize = size
}

// Done stamps the client write-back: the incoming response's
// span since the send, and its body size.
func (e *Entry) Done(size int64) {
	e.InMs = ceilMs(time.Since(e.sent))
	e.InSize = size
}

// ceilMs rounds a span up to whole milliseconds: any nonzero
// span is at least 1.
func ceilMs(d time.Duration) int64 {
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}

// Log appends one object to the record's log list. The
// caller owns the object's keys; no locking is needed, since
// a request and its snippet calls run on one goroutine.
func (e *Entry) Log(values map[string]any) {
	e.Logs = append(e.Logs, values)
}

// SetOutPath records the path sent to the outlet, dropped
// on a match.
func (e *Entry) SetOutPath(out string) {
	if out == e.Path {
		e.OutPath = ""
		return
	}
	e.OutPath = out
}

// SetOutQuery records the query sent to the outlet, dropped
// on a match.
func (e *Entry) SetOutQuery(out string) {
	if out == e.Query {
		e.OutQuery = ""
		return
	}
	e.OutQuery = out
}

// capQuery caps a query at 64 characters, cut at 61 with a ...
// tail beyond: the log line stays short, the truncation plain.
func capQuery(query string) string {
	if len(query) <= 64 {
		return query
	}
	return query[:61] + "..."
}

// SetOutMethod records the method sent to the outlet,
// dropped on a match.
func (e *Entry) SetOutMethod(out string) {
	if out == e.Method {
		e.OutMethod = ""
		return
	}
	e.OutMethod = out
}

// SetInCode records the code that came in from the outlet,
// dropped on a match.
func (e *Entry) SetInCode(in int64) {
	if in == e.Code {
		e.InCode = 0
		return
	}
	e.InCode = in
}

// Write emits one entry as a JSON line. A zero Time is filled
// with now, so the other line kinds need no clock of their own.
// An indent set through SetIndent expands each entry into a
// multi-line object instead. Nothing is written when no writer
// is set.
func Write(e Entry) {
	if e.Time.IsZero() {
		e.Time = Time{time.Now()}
	}
	// The queries cap on the copy: the record keeps its own
	// exact forms, the line stays short.
	e.Query = capQuery(e.Query)
	e.OutQuery = capQuery(e.OutQuery)
	data, err := plainjson.Marshal(e)
	if err != nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if writer == nil {
		return
	}
	if indent != "" {
		indented := &bytes.Buffer{}
		if err := json.Indent(indented, data, "", indent); err != nil {
			return
		}
		data = indented.Bytes()
	}
	_, _ = writer.Write(data)
	_, _ = writer.Write([]byte{'\n'})
}

// Log emits one line from outside any request - a stray
// print, say - as a bare log list holding one object: msg,
// plus the given key/value pairs. A dangling key with no
// value is dropped.
func Log(msg string, pairs ...any) {
	object := make(map[string]any, 1+len(pairs)/2)
	if msg != "" {
		object["msg"] = msg
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		object[fmt.Sprint(pairs[i])] = pairs[i+1]
	}
	Write(Entry{Logs: []map[string]any{object}})
}
