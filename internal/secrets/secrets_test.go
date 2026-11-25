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

package secrets_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/canonical/rebear/internal/secrets"
)

// b64 is the file spelling of one field.
func b64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// seal writes one sealed secret into the directory, reusing a
// shared salt so the tests pay one derivation.
func seal(t *testing.T, dir, name, password, plain string) []byte {
	t.Helper()
	salt := make([]byte, 24)
	body, err := secrets.Encrypt(plain, password, salt)
	if err != nil {
		t.Fatalf("cannot seal %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), body, 0o600); err != nil {
		t.Fatalf("cannot write %s: %v", name, err)
	}
	return body
}

func TestEncryptSpellsTheBody(t *testing.T) {
	// The seal spells the three fields, this order, standard
	// base64: the salt and the nonce hold 24 bytes without a
	// padding character, and the box carries the secret plus
	// its tag. The whole content is one line closed by a
	// newline, the form a text file takes.
	salt := make([]byte, 24)
	salt[0] = 0xab
	body, err := secrets.Encrypt("tok_abc", "pw", salt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(string(body), "\n") || strings.Count(string(body), "\n") != 1 {
		t.Errorf("body = %q, want one line closed by a newline", string(body))
	}
	var fields map[string]string
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("cannot decode the body: %v", err)
	}
	if fields["salt"] != b64(salt) {
		t.Errorf("salt = %q, want %q", fields["salt"], b64(salt))
	}
	for _, name := range []string{"salt", "nonce"} {
		if len(fields[name]) != 32 || strings.HasSuffix(fields[name], "=") {
			t.Errorf("%s = %q, want 32 characters unpadded", name, fields[name])
		}
	}
	plain, err := base64.StdEncoding.DecodeString(fields["box"])
	if err != nil {
		t.Fatalf("cannot decode the box: %v", err)
	}
	if len(plain) != len("tok_abc")+16 {
		t.Errorf("box = %d bytes, want the secret plus its 16-byte tag", len(plain))
	}
	if i := strings.Index(string(body), `"salt"`); i > strings.Index(string(body), `"nonce"`) {
		t.Errorf("body = %q, want salt before nonce", string(body))
	}
}

func TestEncryptRefusesBadSalt(t *testing.T) {
	// The seal seals under the salt the caller picked, and the
	// salt's size is the format's, not an input.
	_, err := secrets.Encrypt("x", "pw", make([]byte, 23))
	if err == nil || !strings.Contains(err.Error(), "salt must be 24 bytes") {
		t.Errorf("error = %v, want the salt size", err)
	}
}

func TestSaltDecodesExact(t *testing.T) {
	// --salt takes the file's own spelling: standard base64
	// decoding to exactly the salt's size.
	salt, err := secrets.Salt(b64(make([]byte, 24)))
	if err != nil || len(salt) != 24 {
		t.Errorf("Salt of 24 bytes = %v, %v", salt, err)
	}
	url := base64.URLEncoding.EncodeToString([]byte{0xfb, 0xff, 0xbf, 0xef})
	testCases := []struct {
		value string
		want  string
	}{{
		value: url,
		want:  "standard base64",
	}, {
		value: b64(make([]byte, 23)),
		want:  "standard base64",
	}, {
		value: b64(make([]byte, 25)),
		want:  "standard base64",
	}, {
		value: "!!!",
		want:  "standard base64",
	}}
	for _, tc := range testCases {
		_, err := secrets.Salt(tc.value)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Salt(%q): error = %v, want %q", tc.value, err, tc.want)
		}
	}
}

func TestScanReadsTheManifest(t *testing.T) {
	// The scan reads the directory as the manifest: file order
	// is name order, the name is the filename without .json,
	// and the fields decode to their bytes. A missing or empty
	// directory is no secrets.
	dir := t.TempDir()
	if sealed, err := secrets.Scan(dir); len(sealed) != 0 || err != nil {
		t.Errorf("empty directory: %v, %v", sealed, err)
	}
	missing := filepath.Join(dir, "missing")
	if sealed, err := secrets.Scan(missing); len(sealed) != 0 || err != nil {
		t.Errorf("missing directory: %v, %v", sealed, err)
	}

	dir = t.TempDir()
	seal(t, dir, "remote", "pw", "tok")
	sealed, err := secrets.Scan(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sealed) != 1 || sealed[0].Name != "remote" || len(sealed[0].Salt) != 24 || len(sealed[0].Nonce) != 24 || len(sealed[0].Box) != 3+16 {
		t.Errorf("sealed = %+v, want the one remote secret", sealed)
	}
}

func TestScanRefusesBrokenInput(t *testing.T) {
	// Everything in the directory is a secret and the format is
	// our own: an entry, a name, or a body that breaks the form
	// fails the scan, naming its file.
	salt24 := b64(make([]byte, 24))
	short23 := b64(make([]byte, 23))
	testCases := []struct {
		name, entry, body string
		want              string
	}{{
		name:  "stray entry",
		entry: "notes.txt",
		want:  "not a json file",
	}, {
		name:  "rule-breaking name",
		entry: "Bad.json",
		body:  `{}`,
		want:  "must match",
	}, {
		name:  "non-object body",
		entry: "remote.json",
		body:  `[]`,
		want:  "cannot unmarshal",
	}, {
		name:  "unknown field",
		entry: "remote.json",
		body:  `{"salt":"` + salt24 + `","nonce":"` + salt24 + `","box":"` + salt24 + `","extra":1}`,
		want:  "unknown field",
	}, {
		name:  "missing field",
		entry: "remote.json",
		body:  `{"salt":"` + salt24 + `","nonce":"` + salt24 + `"}`,
		want:  `missing "box"`,
	}, {
		name:  "short salt",
		entry: "remote.json",
		body:  `{"salt":"` + short23 + `","nonce":"` + salt24 + `","box":"` + salt24 + `"}`,
		want:  `"salt" must decode to 24 bytes`,
	}, {
		name:  "wrong nonce",
		entry: "remote.json",
		body:  `{"salt":"` + salt24 + `","nonce":"` + short23 + `","box":"` + salt24 + `"}`,
		want:  `"nonce" must decode to 24 bytes`,
	}, {
		name:  "box without room for its tag",
		entry: "remote.json",
		body:  `{"salt":"` + salt24 + `","nonce":"` + salt24 + `","box":"` + b64(make([]byte, 15)) + `"}`,
		want:  `must decode to at least 16 bytes`,
	}, {
		name:  "unpadded box",
		entry: "remote.json",
		body:  `{"salt":"` + salt24 + `","nonce":"` + salt24 + `","box":"` + base64.RawStdEncoding.EncodeToString(make([]byte, 17)) + `"}`,
		want:  `must be standard base64`,
	}, {
		name:  "url-safe salt",
		entry: "remote.json",
		body:  `{"salt":"` + base64.URLEncoding.EncodeToString([]byte{0xfb, 0xff, 0xbf, 0xef}) + `","nonce":"` + salt24 + `","box":"` + salt24 + `"}`,
		want:  `must be standard base64`,
	}}
	for _, tc := range testCases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, tc.entry), []byte(tc.body), 0o600); err != nil {
			t.Fatalf("%s: cannot write the entry: %v", tc.name, err)
		}
		sealed, err := secrets.Scan(dir)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.entry) {
			t.Errorf("%s: error = %v, want %q naming %q", tc.name, err, tc.want, tc.entry)
		}
		if len(sealed) != 0 {
			t.Errorf("%s: sealed = %v, want none", tc.name, sealed)
		}
	}
}

func TestOpenRoundTrip(t *testing.T) {
	// The seal opens with the same password: every secret of a
	// shared-salt directory lands, and the plaintext keeps its
	// bytes.
	dir := t.TempDir()
	salt := make([]byte, 24)
	for _, name := range []string{"alpha", "beta"} {
		plain := "plain-" + name
		body, err := secrets.Encrypt(plain, "pw", salt)
		if err != nil {
			t.Fatalf("cannot seal %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), body, 0o600); err != nil {
			t.Fatalf("cannot write %s: %v", name, err)
		}
	}
	sealed, err := secrets.Scan(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	opened, err := secrets.Open(sealed, "pw")
	if err != nil {
		t.Fatalf("cannot open: %v", err)
	}
	if opened["alpha"] != "plain-alpha" || opened["beta"] != "plain-beta" {
		t.Errorf("opened = %v, want both plaintexts", opened)
	}
}

func TestOpenWrongPassword(t *testing.T) {
	// The wrong password fails at the first secret in scan
	// order, naming it.
	dir := t.TempDir()
	salt := make([]byte, 24)
	for _, name := range []string{"alpha", "beta"} {
		body, err := secrets.Encrypt("plain", "pw", salt)
		if err != nil {
			t.Fatalf("cannot seal %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), body, 0o600); err != nil {
			t.Fatalf("cannot write %s: %v", name, err)
		}
	}
	sealed, err := secrets.Scan(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	opened, err := secrets.Open(sealed, "wrong")
	if err == nil || !strings.Contains(err.Error(), `secret "alpha": cannot decrypt`) {
		t.Fatalf("error = %v, want alpha named", err)
	}
	if opened != nil {
		t.Errorf("opened = %v, want nil", opened)
	}
}

func TestOpenMixedSalts(t *testing.T) {
	// The fallback stays per salt: a directory of files sealed
	// under different salts opens too, paying one derivation
	// per salt.
	dir := t.TempDir()
	names := []string{"alpha", "beta"}
	salts := [][]byte{make([]byte, 24), make([]byte, 24)}
	salts[1][0] = 1
	for i, name := range names {
		body, err := secrets.Encrypt("plain"+strings.Repeat("x", i+1), "pw", salts[i])
		if err != nil {
			t.Fatalf("cannot seal %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), body, 0o600); err != nil {
			t.Fatalf("cannot write %s: %v", name, err)
		}
	}
	sealed, err := secrets.Scan(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opened, err := secrets.Open(sealed, "pw"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if opened["alpha"] != "plainx" || opened["beta"] != "plainxx" {
		t.Errorf("opened = %v, want both plaintexts", opened)
	}
}
