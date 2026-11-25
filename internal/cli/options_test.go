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

func TestParseAddSecret(t *testing.T) {
	// --add-secret takes one name, spelled apart or with =; a
	// second one is an error.
	opts, err := Parse([]string{"--add-secret", "api"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.AddSecret != "api" {
		t.Errorf("AddSecret = %q, want api", opts.AddSecret)
	}
	opts, err = Parse([]string{"--add-secret=api"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.AddSecret != "api" {
		t.Errorf("AddSecret = %q, want api", opts.AddSecret)
	}
	testCases := []struct {
		argv []string
		want string
	}{{
		argv: []string{"--add-secret"},
		want: "needs a name",
	}, {
		argv: []string{"--add-secret", "api", "--add-secret", "beta"},
		want: "accepts one name",
	}, {
		argv: []string{"--add-secret=api", "--add-secret=beta"},
		want: "accepts one name",
	}}
	for _, tc := range testCases {
		_, err := Parse(tc.argv)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Parse(%v): error = %v, want %q", tc.argv, err, tc.want)
		}
	}
}

func TestParseSalt(t *testing.T) {
	// --salt takes one value and belongs to the add: alone it
	// is an error.
	opts, err := Parse([]string{"--add-secret", "api", "--salt", "AA=="})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Salt != "AA==" || opts.AddSecret != "api" {
		t.Errorf("opts = %+v, want both values", opts)
	}
	_, err = Parse([]string{"--salt=AA=="})
	if err == nil || !strings.Contains(err.Error(), "--salt needs --add-secret") {
		t.Errorf("error = %v, want the --add-secret requirement", err)
	}
	_, err = Parse([]string{"--add-secret", "api", "--salt", "AA==", "--salt", "BB=="})
	if err == nil || !strings.Contains(err.Error(), "accepts one value") {
		t.Errorf("error = %v, want one value only", err)
	}
}

func TestParseKeyring(t *testing.T) {
	// --keyring reads the password from the keyring; without it
	// the asks read stdin. --set-keyring stores the entry and
	// exits. The two exclude each other and the other one-shot
	// operations: one password flow per run.
	opts, err := Parse([]string{"--keyring"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !opts.Keyring {
		t.Errorf("Keyring = false, want true")
	}
	opts, err = Parse([]string{"--set-keyring"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !opts.SetKeyring {
		t.Errorf("SetKeyring = false, want true")
	}
	opts, err = Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Keyring || opts.SetKeyring {
		t.Errorf("opts = %+v, want the defaults", opts)
	}
	testCases := []struct {
		argv []string
		want string
	}{{
		argv: []string{"--keyring", "--set-keyring"},
		want: "--set-keyring cannot be used with --keyring",
	}, {
		argv: []string{"--add-secret", "api", "--set-keyring"},
		want: "--set-keyring cannot be used with --add-secret",
	}, {
		argv: []string{"--add-tls", "svc", "--set-keyring"},
		want: "--set-keyring cannot be used with --add-tls",
	}}
	for _, tc := range testCases {
		_, err := Parse(tc.argv)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Parse(%v): error = %v, want %q", tc.argv, err, tc.want)
		}
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
