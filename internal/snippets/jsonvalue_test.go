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

package snippets_test

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/canonical/rebear/internal/setup"
	"github.com/canonical/rebear/internal/snippets"
)

// outgoingBody runs the sniplet's outgoing filter on a fresh value
// holding the body, and returns the bytes the value produces.
func outgoingBody(t *testing.T, program *snippets.Program, flow *snippets.Flow, sniplet setup.Sniplet, body string) (string, error) {
	t.Helper()
	value := snippets.NewJSONValue([]byte(body))
	if err := program.OutgoingBody(flow, sniplet, value); err != nil {
		return "", err
	}
	got, err := value.Bytes()
	if err != nil {
		return "", err
	}
	return string(got), nil
}

// incomingBody runs the sniplet's incoming filter the same way.
func incomingBody(t *testing.T, program *snippets.Program, flow *snippets.Flow, sniplet setup.Sniplet, body string) (string, error) {
	t.Helper()
	value := snippets.NewJSONValue([]byte(body))
	if err := program.IncomingBody(flow, sniplet, value); err != nil {
		return "", err
	}
	got, err := value.Bytes()
	if err != nil {
		return "", err
	}
	return string(got), nil
}

func TestJSONRunsPerEntity(t *testing.T) {
	// The body filter runs one entity's snippet on the
	// parsed dict and re-encodes: deletes survive and numbers
	// arrive as ints.
	cfg, inlet, _ := config()
	inlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|body.json.pop("keep", None)
		|if body.json["count"] != 3:
		|    fail("count is not an int")
		|body.json["count"] += 1
	`)}
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := outgoingBody(t, program, flow, inlet, `{"keep":"yes","junk":"bye","count":3}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `{"junk":"bye","count":4}`
	if got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestIncomingBodySharesFlowState(t *testing.T) {
	// The response body filter shares the flow state with
	// the outlet's opening: the opening records, the filter reads.
	cfg, inlet, outlet := config()
	outlet.OpeningFlow = reindent(`
		|flow.state["seen"] = "yes"
	`)
	outlet.IncomingBody = &setup.BodyFilter{Body: reindent(`
		|if flow.state["seen"] != "yes":
		|    fail("opening did not run first")
		|body.json["order"] = "ok"
	`)}
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := program.OpeningFlow(flow, outlet, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := incomingBody(t, program, flow, outlet, `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != `{"order":"ok"}` {
		t.Errorf("body = %q, want the outlet's change", got)
	}
}

func TestJSONPassesNonJSONThrough(t *testing.T) {
	// A body that is not JSON at all still reaches the snippet:
	// body.json reads None, body.data the raw bytes, and the body
	// passes through unchanged unless the snippet assigns.
	cfg, inlet, outlet := config()
	inlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|if body.json != None:
		|    fail("body.json not None")
		|if type(body.data) != "string":
		|    fail("body.data not the raw bytes")
	`)}
	outlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|if body.json != None:
		|    fail("body.json not None")
	`)}
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := program.OpeningFlow(flow, outlet, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, body := range []string{
		"",
		"\x1fraw\x00bytes",
		"{\"a\": 1} trailing",
	} {
		got, err := outgoingBody(t, program, flow, inlet, body)
		if err != nil {
			t.Fatalf("body %q: unexpected error: %v", body, err)
		}
		if got != body {
			t.Errorf("body %q passed as %q, want unchanged", body, got)
		}
	}
}

func TestJSONRunsOnEveryKind(t *testing.T) {
	// Every JSON kind reaches the snippet as its starlark value:
	// a dict and a list mutate, the scalars arrive to observe.
	cfg, inlet, _ := config()
	inlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|kind = type(body.json)
		|if kind not in ("dict", "list", "string", "int", "float", "bool", "NoneType"):
		|    fail("unexpected kind: " + kind)
	`)}
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Dict and list cases with per-kind snippets below; the
	// scalar kinds go through the same value argument.
	for _, body := range []string{
		`{"n": 1}`,
		`[1, 2]`,
		`"text"`,
		`3`,
		`2.5`,
		`true`,
		`null`,
	} {
		if _, err := outgoingBody(t, program, flow, inlet, body); err != nil {
			t.Fatalf("body %q: unexpected error: %v", body, err)
		}
	}
}

func TestJSONListMutatesInPlace(t *testing.T) {
	// A list body crosses two stages sharing the value: the
	// second snippet sees the first's mutation, and the bytes
	// are produced once, at the end.
	cfg, inlet, outlet := config()
	inlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|if type(body.json) != "list":
		|    fail("not a list")
		|body.json.append("inlet")
	`)}
	outlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|if "inlet" not in body.json:
		|    fail("inlet's change missing: value not shared")
		|body.json.append("outlet")
	`)}
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	value := snippets.NewJSONValue([]byte(`[0]`))
	if err := program.OutgoingBody(flow, inlet, value); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := program.OutgoingBody(flow, outlet, value); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := value.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := `[0,"inlet","outlet"]`; string(got) != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestJSONValueKinds(t *testing.T) {
	// Kind reports what the first rune promises; it does not
	// guarantee the value is valid. The typed accessors answer
	// ErrValueType on a mismatch, without storing it.
	testCases := []struct {
		body string
		kind snippets.Kind
	}{
		{"", snippets.NoKind},
		{"  \t\n", snippets.NoKind},
		{"\x1fraw", snippets.NoKind},
		{`  {"a": 1}`, snippets.DictKind},
		{`[1]`, snippets.ListKind},
		{`"s"`, snippets.StringKind},
		{"-3", snippets.NumberKind},
		{"2.5", snippets.NumberKind},
		{"true", snippets.BoolKind},
		{"false", snippets.BoolKind},
		{"null", snippets.NullKind},
	}
	for _, tc := range testCases {
		value := snippets.NewJSONValue([]byte(tc.body))
		if got := value.Kind(); got != tc.kind {
			t.Errorf("Kind() of %q = %v, want %v", tc.body, got, tc.kind)
		}
	}
	value := snippets.NewJSONValue([]byte(`[1, 2]`))
	if _, err := value.Dict(); !errors.Is(err, snippets.ErrValueType) {
		t.Errorf("Dict() on a list = %v, want ErrValueType", err)
	}
	if err := value.Err(); err != nil {
		t.Errorf("Err() = %v, want nil: ErrValueType is not stored", err)
	}
	if str, err := value.String(); !errors.Is(err, snippets.ErrValueType) || str != "" {
		t.Errorf("String() on a list = (%q, %v), want (\"\", ErrValueType)", str, err)
	}
	list, err := value.List()
	if err != nil {
		t.Fatalf("List() on a list: %v", err)
	}
	if got := list.Len(); got != 2 {
		t.Errorf("list length = %d, want 2", got)
	}
	if _, err := value.Number(); !errors.Is(err, snippets.ErrValueType) {
		t.Errorf("Number() on a list = %v, want ErrValueType", err)
	}
	if _, err := value.Bool(); !errors.Is(err, snippets.ErrValueType) {
		t.Errorf("Bool() on a list = %v, want ErrValueType", err)
	}
	if err := value.Null(); !errors.Is(err, snippets.ErrValueType) {
		t.Errorf("Null() on a list = %v, want ErrValueType", err)
	}
	num := snippets.NewJSONValue([]byte("42"))
	n, err := num.Number()
	if err != nil {
		t.Fatalf("Number() on 42: %v", err)
	}
	if n.String() != "42" {
		t.Errorf("Number() on 42 = %s, want 42", n.String())
	}
	b := snippets.NewJSONValue([]byte("true"))
	got, err := b.Bool()
	if err != nil || got != true {
		t.Errorf("Bool() on true = (%v, %v), want (true, nil)", got, err)
	}
	nul := snippets.NewJSONValue([]byte("null"))
	if err := nul.Null(); err != nil {
		t.Errorf("Null() on null = %v, want nil", err)
	}
	str := snippets.NewJSONValue([]byte(`"text"`))
	s, err := str.String()
	if err != nil || s != "text" {
		t.Errorf("String() = (%q, %v), want (\"text\", nil)", s, err)
	}
}

func TestJSONKeepsKeyOrder(t *testing.T) {
	// The re-encode keeps the body's key order: an untouched
	// body comes out byte for byte, without filter churn.
	cfg, inlet, _ := config()
	inlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|if type(body.json) != "dict":
		|    fail("value is not a dict")
		|flow.state["seen"] = "yes"
	`)}
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	value := snippets.NewJSONValue([]byte(`{"z":1,"a":{"z":[true,null,"x"],"a":2.5},"n":-3}`))
	if err := program.OutgoingBody(flow, inlet, value); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := value.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `{"z":1,"a":{"z":[true,null,"x"],"a":2.5},"n":-3}`
	if string(got) != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestJSONOverLimitFails(t *testing.T) {
	// The filter backstop holds the same rule the proxy
	// enforces at the buffer: a body over JSONLimit fails at
	// the body.json read, the message naming both sizes.
	cfg, inlet, _ := config()
	inlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|_ = body.json
	`)}
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := []byte("{\"")
	for len(body) <= snippets.JSONLimit {
		body = append(body, 'a')
	}
	value := snippets.NewJSONValue(body)
	err = program.OutgoingBody(flow, inlet, value)
	want := "json body of " + strconv.Itoa(len(body)) + " bytes exceeds the " + strconv.Itoa(snippets.JSONLimit) + " byte limit"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if value.Err() == nil || !strings.Contains(value.Err().Error(), want) {
		t.Errorf("Err() = %v, want the stored failure", value.Err())
	}
}

func TestJSONValueSharedAcrossStages(t *testing.T) {
	// One value crosses two filtering stages: the second snippet
	// sees the first's mutation, so the dict is shared, and the
	// bytes are produced once, at the end.
	cfg, inlet, outlet := config()
	inlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|body.json["stage"] = "inlet"
	`)}
	outlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|if body.json["stage"] != "inlet":
		|    fail("inlet's change missing: dict not shared")
		|body.json["stage"] = "outlet"
	`)}
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	value := snippets.NewJSONValue([]byte(`{}`))
	if err := program.OutgoingBody(flow, inlet, value); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := program.OutgoingBody(flow, outlet, value); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := value.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := `{"stage":"outlet"}`; string(got) != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	again, err := value.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(again, got) {
		t.Errorf("second Bytes() = %q, want the cached %q", again, got)
	}
}

func TestJSONValueWithoutDecodedForm(t *testing.T) {
	// A body that is not JSON at all answers ErrValueType without
	// storing it: the original bytes still come back, and the
	// value stays usable. A JSON body of the wrong kind answers
	// ErrValueType from the typed accessors too - also unstored.
	value := snippets.NewJSONValue([]byte("raw \x00 bytes"))
	if _, err := value.Dict(); !errors.Is(err, snippets.ErrValueType) {
		t.Errorf("Dict() error = %v, want ErrValueType", err)
	}
	got, err := value.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "raw \x00 bytes" {
		t.Errorf("body = %q, want the original bytes", got)
	}
	if err := value.Err(); err != nil {
		t.Errorf("Err() = %v, want nil: ErrValueType is not stored", err)
	}
	list := snippets.NewJSONValue([]byte("[1, 2]"))
	if _, err := list.Dict(); !errors.Is(err, snippets.ErrValueType) {
		t.Errorf("Dict() on a list = %v, want ErrValueType", err)
	}
	if err := list.Err(); err != nil {
		t.Errorf("Err() = %v, want nil: ErrValueType is not stored", err)
	}
}

func TestBodyAssignNull(t *testing.T) {
	// Assigning None to body.json writes a JSON null body, while
	// a snippet that never assigns leaves the body as-is. None
	// encodes as null inside containers too, and logs as null.
	cfg, inlet, _ := config()
	inlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|body.json = None
	`)}
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := outgoingBody(t, program, flow, inlet, `{"old": true}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != `null` {
		t.Errorf("body = %q, want %q", got, "null")
	}

	inlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|_ = body.json
	`)}
	program = compile(t, cfg)
	flow, err = program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err = outgoingBody(t, program, flow, inlet, `{"keep": true}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != `{"keep":true}` {
		t.Errorf("body = %q, want it untouched", got)
	}

	inlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|body.json = {"x": None, "y": [None, None]}
	`)}
	program = compile(t, cfg)
	flow, err = program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err = outgoingBody(t, program, flow, inlet, `null`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := `{"x":null,"y":[null,null]}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}

	// A null body reads back as None, so the identity assignment
	// round-trips: None writes a JSON null like Null does.
	inlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|if type(body.json) != "NoneType":
		|    fail("null body not decoded as None")
		|body.json = body.json
	`)}
	program = compile(t, cfg)
	flow, err = program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err = outgoingBody(t, program, flow, inlet, `null`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != `null` {
		t.Errorf("body = %q, want %q", got, "null")
	}
}

func TestJSONKeyBindingsNotImplemented(t *testing.T) {
	// The key-binding form of a body filter would stream;
	// until that phase exists, it fails at startup rather
	// than passing by silently.
	cfg, inlet, _ := config()
	inlet.OutgoingBody = &setup.BodyFilter{
		Keys: []setup.KeyBinding{{Path: []string{"choices"}, Snippet: "pass"}},
	}
	_, err := snippets.Compile(cfg)
	want := "inlets.local.outgoing-body: key bindings are not implemented yet"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestBodyAssignReplacesValue(t *testing.T) {
	// Assigning body.json replaces the body, kind changes
	// included; a snippet that never assigns leaves it as-is.
	cfg, inlet, _ := config()
	inlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|body.json = {"wrapped": body.json}
	`)}
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// A string body: the snippet observes it and wraps it in
	// a dict.
	got, err := outgoingBody(t, program, flow, inlet, `"hello"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := `{"wrapped":"hello"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	// A dict body: the assignment replaces it wholesale.
	got, err = outgoingBody(t, program, flow, inlet, `{"old": true}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := `{"wrapped":{"old":true}}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestBodyAssignSharedAcrossStages(t *testing.T) {
	// An assignment feeds the next stage: the inlet's fresh list
	// is what the outlet's snippet appends to.
	cfg, inlet, outlet := config()
	inlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|body.json = [body.json]
	`)}
	outlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|if type(body.json) != "list":
		|    fail("inlet's assignment did not replace the value")
		|body.json.append("outlet")
	`)}
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	value := snippets.NewJSONValue([]byte(`"body"`))
	if err := program.OutgoingBody(flow, inlet, value); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := program.OutgoingBody(flow, outlet, value); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := value.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := `["body","outlet"]`; string(got) != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestBodyAssignUnencodableFails(t *testing.T) {
	// An assignment the encoder cannot write fails terminally at
	// Bytes(), the stored failure visible through Err().
	cfg, inlet, _ := config()
	inlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|body.json = flow
	`)}
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	value := snippets.NewJSONValue([]byte(`{}`))
	if err := program.OutgoingBody(flow, inlet, value); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := value.Bytes(); err == nil {
		t.Fatalf("Bytes() = nil error, want the encode failure")
	}
	if value.Err() == nil || !strings.Contains(value.Err().Error(), "cannot encode") {
		t.Errorf("Err() = %v, want the stored encode failure", value.Err())
	}
	if got, err := value.Bytes(); err == nil || got != nil {
		t.Errorf("second Bytes() = (%q, %v), want the terminal failure again", got, err)
	}
}

func TestBodyArgument(t *testing.T) {
	// The body arrives as a body value: body.json is the decoded
	// form, mutations through it persist to the bytes, and other
	// attributes do not exist.
	testCases := []struct {
		snippet string
		body    string
		want    string
		wantErr string
	}{{
		snippet: "if type(body) != \"body\":\n    fail(\"not a body value\")\nbody.json[\"n\"] = 2",
		body:    `{"n":1}`,
		want:    `{"n":2}`,
	}, {
		snippet: "if body.data != \"plain\":\n    fail(\"body.data not the raw bytes\")",
		body:    "plain",
		want:    "plain",
	}, {
		snippet: "body.json[\"n\"] = 2\nif body.data != \"{\\\"n\\\":2}\":\n    fail(\"body.data not re-encoded\")",
		body:    `{"n":1}`,
		want:    `{"n":2}`,
	}, {
		snippet: "body.data = \"rewritten\"",
		body:    `{"n":1}`,
		want:    "rewritten",
	}, {
		// A JSON string assigned to body.data decodes through a
		// later body.json read: the kind re-senses at the set.
		snippet: "body.data = \"{\\\"n\\\":2}\"\nif body.json[\"n\"] != 2:\n    fail(\"body.json not decoded from the new data\")\nbody.json[\"n\"] = 3",
		body:    `{"n":1}`,
		want:    `{"n":3}`,
	}, {
		snippet: "body.json = None",
		body:    `{}`,
		want:    `null`,
	}, {
		snippet: "body.data = 1",
		body:    `{}`,
		wantErr: "cannot set body.data: got int, want string",
	}, {
		// The event fields left the body: reading or assigning
		// one on a body fails, loudly.
		snippet: "body.name = \"x\"",
		body:    `{}`,
		wantErr: "body has no .name field or method",
	}, {
		snippet: "body.event = \"x\"",
		body:    `{}`,
		wantErr: "body has no .event field or method",
	}, {
		snippet: "body.nope",
		body:    `{"n":1}`,
		wantErr: "body has no .nope field or method",
	}}
	for _, tc := range testCases {
		cfg, inlet, _ := config()
		inlet.OutgoingBody = &setup.BodyFilter{Body: tc.snippet}
		program := compile(t, cfg)
		flow, err := program.OpeningFlow(nil, inlet, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, err := outgoingBody(t, program, flow, inlet, tc.body)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != tc.want {
			t.Errorf("body = %q, want %q", got, tc.want)
		}
	}
}
