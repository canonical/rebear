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
	"net/http"
	"strings"
	"testing"

	"github.com/canonical/rebear/internal/setup"
	"github.com/canonical/rebear/internal/snippets"
)

// runOutgoing seeds a flow with the raw query and runs the inlet's
// outgoing-headers snippet on it, the proxy's own order.
func runOutgoing(t *testing.T, inlet *setup.Inlet, rawQuery string) *snippets.Flow {
	t.Helper()
	program := compile(t, &setup.Setup{Inlets: []*setup.Inlet{inlet}})
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("GET", "/x", rawQuery, http.Header{})
	if err := program.OutgoingHeaders(flow, inlet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return flow
}

func TestQueryReadsTheSeed(t *testing.T) {
	// The readings answer the first item of each parameter,
	// sorted as the encoding writes them; reading alone never
	// marks the query updated.
	inlet := &setup.Inlet{Name: "local", OutgoingHeaders: reindent(`
		|if outgoing.query["b"] != "2":
		|    fail("index read not the first value")
		|if outgoing.query.get("a") != "1":
		|    fail("get not the first value")
		|if outgoing.query.get("miss") != None:
		|    fail("a miss must default to None")
		|if outgoing.query.get("miss", "dflt") != "dflt":
		|    fail("the default must answer")
		|if outgoing.query.keys() != ["a", "b"]:
		|    fail("keys not sorted")
		|if outgoing.query.values() != ["1", "2"]:
		|    fail("values not the first items, sorted")
		|if len(outgoing.query) != 2:
		|    fail("len not the key count")
		|for key in outgoing.query:
		|    if key != "a" and key != "b":
		|        fail("iteration not over the keys")
	`)}
	flow := runOutgoing(t, inlet, "b=2&b=3&a=1")
	if flow.QueryUpdated() {
		t.Errorf("QueryUpdated = true, want false: reading is no mutation")
	}
}

func TestQueryNoneBeforeSeed(t *testing.T) {
	// An unseeded flow models no request: the query reads None
	// with the other fields.
	cfg, inlet, _ := config()
	inlet.OpeningFlow = reindent(`
		|if outgoing.query != None:
		|    fail("unseeded query must read None")
	`)
	program := compile(t, cfg)
	if _, err := program.OpeningFlow(nil, inlet, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryIndexWrites(t *testing.T) {
	// The index assignment accepts a single string and a list
	// of strings, and marks the query updated either way.
	inlet := &setup.Inlet{Name: "local", OutgoingHeaders: reindent(`
		|outgoing.query["a"] = "one"
		|outgoing.query["b"] = ["x", "y"]
		|if outgoing.query.getn("b") != ["x", "y"]:
		|    fail("the list did not land whole")
	`)}
	flow := runOutgoing(t, inlet, "a=1&b=2")
	if !flow.QueryUpdated() {
		t.Errorf("QueryUpdated = false, want true on the assignment")
	}
	if got := flow.OutQuery().Encode(); got != "a=one&b=x&b=y" {
		t.Errorf("Encode = %q, want a=one&b=x&b=y", got)
	}
}

func TestQueryIndexMissingKeyFails(t *testing.T) {
	// A missing key fails the index read, as it does on a
	// dict: get answers the default case.
	inlet := &setup.Inlet{Name: "local", OutgoingHeaders: reindent(`
		|x = outgoing.query["miss"]
	`)}
	program := compile(t, &setup.Setup{Inlets: []*setup.Inlet{inlet}})
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("GET", "/x", "a=1", http.Header{})
	err = program.OutgoingHeaders(flow, inlet)
	if err == nil || !strings.Contains(err.Error(), `key "miss" not in query`) {
		t.Fatalf("error = %v, want the missing key failure", err)
	}
}

func TestQueryPopAndFriends(t *testing.T) {
	// pop removes a key and answers its first value, or the
	// default on a miss and a failure without one; popitem
	// removes the first sorted key; setdefault sets only a
	// missing key.
	inlet := &setup.Inlet{Name: "local", OutgoingHeaders: reindent(`
		|if outgoing.query.pop("b") != "2":
		|    fail("pop not the first value")
		|if outgoing.query.pop("miss", "dflt") != "dflt":
		|    fail("pop default not answered")
		|if outgoing.query.setdefault("a", "new") != "1":
		|    fail("setdefault answered a present key's default")
		|if outgoing.query.setdefault("c", "new") != "new":
		|    fail("setdefault did not set the missing key")
		|if outgoing.query.popitem() != ("a", "1"):
		|    fail("popitem not the first sorted pair")
		|if outgoing.query.keys() != ["c"]:
		|    fail("the removals did not land")
	`)}
	flow := runOutgoing(t, inlet, "b=2&b=3&a=1")
	if !flow.QueryUpdated() {
		t.Errorf("QueryUpdated = false, want true on the removals")
	}
}

func TestQueryPopFailures(t *testing.T) {
	// A pop without a default on a miss fails, and so does a
	// popitem on an empty query.
	inlet := &setup.Inlet{Name: "local", OutgoingHeaders: reindent(`
		|outgoing.query.pop("miss")
	`)}
	program := compile(t, &setup.Setup{Inlets: []*setup.Inlet{inlet}})
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("GET", "/x", "a=1", http.Header{})
	if err := program.OutgoingHeaders(flow, inlet); err == nil || !strings.Contains(err.Error(), "pop: missing key") {
		t.Fatalf("error = %v, want the missing key failure", err)
	}

	inlet.OutgoingHeaders = reindent(`
		|outgoing.query.popitem()
	`)
	program = compile(t, &setup.Setup{Inlets: []*setup.Inlet{inlet}})
	flow, err = program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("GET", "/x", "", http.Header{})
	err = program.OutgoingHeaders(flow, inlet)
	if err == nil || !strings.Contains(err.Error(), "popitem: empty query") {
		t.Fatalf("error = %v, want the empty query failure", err)
	}
}

func TestQueryListVariants(t *testing.T) {
	// The n variants answer the whole values lists.
	inlet := &setup.Inlet{Name: "local", OutgoingHeaders: reindent(`
		|if outgoing.query.getn("b") != ["2", "3"]:
		|    fail("getn not the whole list")
		|if outgoing.query.getn("miss") != []:
		|    fail("a miss must answer an empty list")
		|if outgoing.query.popn("b") != ["2", "3"]:
		|    fail("popn not the whole list")
		|if outgoing.query.popitemn() != ("a", ["1"]):
		|    fail("popitemn not the pair with the list")
	`)}
	runOutgoing(t, inlet, "b=2&b=3&a=1")
}

func TestQueryUpdate(t *testing.T) {
	// update takes a dict, a list of pairs, and the keyword
	// arguments, single values and lists alike.
	inlet := &setup.Inlet{Name: "local", OutgoingHeaders: reindent(`
		|outgoing.query.update({"a": "one"}, b=["x", "y"])
		|outgoing.query.update([("c", "3")])
	`)}
	flow := runOutgoing(t, inlet, "a=1&b=2")
	if got := flow.OutQuery().Encode(); got != "a=one&b=x&b=y&c=3" {
		t.Errorf("Encode = %q, want a=one&b=x&b=y&c=3", got)
	}
}

func TestQueryRefusesAfterSend(t *testing.T) {
	// Once the request left, every mutation refuses with the
	// already sent failure.
	inlet := &setup.Inlet{Name: "local", OutgoingHeaders: reindent(`
		|outgoing.query["a"] = "1"
	`)}
	program := compile(t, &setup.Setup{Inlets: []*setup.Inlet{inlet}})
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("GET", "/x", "a=0", http.Header{})
	flow.LockOut()
	err = program.OutgoingHeaders(flow, inlet)
	if err == nil || !strings.Contains(err.Error(), "cannot mutate outgoing.query: request already sent") {
		t.Fatalf("error = %v, want the already sent failure", err)
	}
}

func TestQueryBadValuesFail(t *testing.T) {
	// A non-string value and a non-string list element fail,
	// naming the query form wanted.
	inlet := &setup.Inlet{Name: "local", OutgoingHeaders: reindent(`
		|outgoing.query["a"] = 1
	`)}
	program := compile(t, &setup.Setup{Inlets: []*setup.Inlet{inlet}})
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("GET", "/x", "", http.Header{})
	err = program.OutgoingHeaders(flow, inlet)
	if err == nil || !strings.Contains(err.Error(), "query value must be string or list of strings, got int") {
		t.Fatalf("error = %v, want the value form failure", err)
	}
}
