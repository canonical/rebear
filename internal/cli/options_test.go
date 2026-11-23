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
	"errors"
	"strings"
	"testing"
)

func TestParseEmptyArgv(t *testing.T) {
	// With no arguments the defaults hold.
	opts, err := Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Indent != "" || opts.LogAt {
		t.Errorf("opts = %+v, want the defaults", opts)
	}
}

func TestParseIndentFlag(t *testing.T) {
	// --indent is boolean: the padding is fixed at four spaces,
	// and without the flag the one-line form holds.
	opts, err := Parse([]string{"--indent"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Indent != "    " {
		t.Errorf("Indent = %q, want four spaces", opts.Indent)
	}
	opts, err = Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Indent != "" {
		t.Errorf("Indent = %q, want empty", opts.Indent)
	}
}

func TestParseLogAtFlag(t *testing.T) {
	// --log-at is boolean: it records snippet call positions,
	// and without it the positions stay off.
	opts, err := Parse([]string{"--log-at"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !opts.LogAt {
		t.Errorf("LogAt = false, want true")
	}
	opts, err = Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.LogAt {
		t.Errorf("LogAt = true, want false")
	}
}

func TestParseAddTLS(t *testing.T) {
	// --add-tls takes a name list, spelled apart or with =, and
	// repeats accumulate.
	opts, err := Parse([]string{"--add-tls", "foo,bar"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opts.AddTLS) != 2 || opts.AddTLS[0] != "foo" || opts.AddTLS[1] != "bar" {
		t.Errorf("AddTLS = %v, want [foo bar]", opts.AddTLS)
	}
	opts, err = Parse([]string{"--add-tls=foo", "--add-tls=bar"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opts.AddTLS) != 2 || opts.AddTLS[0] != "foo" || opts.AddTLS[1] != "bar" {
		t.Errorf("AddTLS = %v, want [foo bar]", opts.AddTLS)
	}
	_, err = Parse([]string{"--add-tls"})
	if err == nil || !strings.Contains(err.Error(), "needs a name list") {
		t.Errorf("error = %v, want the missing list", err)
	}
}

func TestParseHelp(t *testing.T) {
	// -h and --help ask for the usage text rather than starting
	// up.
	for _, flag := range []string{"-h", "--help"} {
		_, err := Parse([]string{flag})
		var help *HelpRequested
		if !errors.As(err, &help) {
			t.Fatalf("%s: error = %v, want HelpRequested", flag, err)
		}
	}
}

func TestParseUnexpectedArgument(t *testing.T) {
	// Positional arguments are not part of the surface.
	_, err := Parse([]string{"nope"})
	if err == nil || !strings.Contains(err.Error(), `unexpected argument`) {
		t.Fatalf("error = %v, want unexpected argument", err)
	}
}

func TestParseUnknownFlag(t *testing.T) {
	// Unknown flags are rejected with a pointer to the help; -c
	// is unknown now that the setup path comes from the environment.
	for _, flag := range []string{"-z", "-c"} {
		_, err := Parse([]string{flag})
		if err == nil || !strings.Contains(err.Error(), `unknown flag "`+flag+`"`) {
			t.Fatalf("%s: error = %v, want unknown flag", flag, err)
		}
	}
}
