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

// Package secrets seals plaintexts with a password and stores
// them as one JSON file each under the secrets directory. It is
// the only package that touches scrypt and nacl/secretbox, and
// the owner of the whole storage, filename to plaintext.
package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/crypto/scrypt"

	"github.com/canonical/rebear/internal/setup"
)

// The sealing constants: scrypt's parameters, the sizes of the
// salt, the nonce, and the key, and the box tag. They are code
// constants, never file content, so changing one is a format
// change: files sealed by an older build stop opening.
const (
	saltSize    = 24
	nonceSize   = 24
	keySize     = 32
	scryptN     = 32768
	scryptR     = 8
	scryptP     = 1
	boxOverhead = 16
)

// Secret is one sealed value: the name its file carries, and
// the salt, the nonce, and the box the seal is spelled with.
// The plaintext only lives in an Open result.
type Secret struct {
	Name  string
	Salt  []byte
	Nonce []byte
	Box   []byte
}

// sealed is the file body's schema: three standard base64
// fields, in this order, unknown fields refused. A nil field
// is a missing one.
type sealed struct {
	Salt  *string `json:"salt"`
	Nonce *string `json:"nonce"`
	Box   *string `json:"box"`
}

// Salt decodes a --salt value: the same spelling a file's salt
// carries, decoded to exactly the salt's own size.
func Salt(value string) ([]byte, error) {
	salt, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(salt) != saltSize {
		return nil, fmt.Errorf("must be the standard base64 of %d bytes", saltSize)
	}
	return salt, nil
}

// Scan reads the directory as the manifest of the secrets, in
// name order. Every entry must be a readable <name>.json file
// whose name follows the sniplet rule and whose body strictly
// decodes: an editor's stray backup fails loudly, naming its
// file. A missing directory is no secrets, not an error.
func Scan(dir string) ([]Secret, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var found []Secret
	for _, entry := range entries {
		name := entry.Name()
		base, isJSON := strings.CutSuffix(name, ".json")
		if !isJSON {
			return nil, fmt.Errorf("secret %q: not a json file", name)
		}
		if err := setup.CheckName(base); err != nil {
			return nil, fmt.Errorf("secret %q: %v", name, err)
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("secret %q: %v", name, err)
		}
		secret, err := decode(base, data)
		if err != nil {
			return nil, fmt.Errorf("secret %q: %v", name, err)
		}
		found = append(found, secret)
	}
	return found, nil
}

// decode strictly decodes one file's body, before any password
// exists: a broken file fails at load, never at the ask.
func decode(name string, data []byte) (Secret, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var body sealed
	if err := dec.Decode(&body); err != nil {
		return Secret{}, fmt.Errorf("%v", strings.TrimPrefix(err.Error(), "json: "))
	}
	var fields []string
	if body.Salt == nil {
		fields = append(fields, `"salt"`)
	}
	if body.Nonce == nil {
		fields = append(fields, `"nonce"`)
	}
	if body.Box == nil {
		fields = append(fields, `"box"`)
	}
	if len(fields) > 0 {
		return Secret{}, fmt.Errorf("missing %s", strings.Join(fields, ", "))
	}
	salt, err := field("salt", *body.Salt, saltSize, saltSize)
	if err != nil {
		return Secret{}, err
	}
	nonce, err := field("nonce", *body.Nonce, nonceSize, nonceSize)
	if err != nil {
		return Secret{}, err
	}
	box, err := field("box", *body.Box, boxOverhead, 0)
	if err != nil {
		return Secret{}, err
	}
	return Secret{Name: name, Salt: salt, Nonce: nonce, Box: box}, nil
}

// field decodes one standard base64 field, refusing the unpadded
// spelling, and checks its length against the exact want; a
// min of zero takes any length at or above it.
func field(name, value string, min, exact int) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("%q must be standard base64: %v", name, err)
	}
	if exact > 0 && len(data) != exact || exact == 0 && len(data) < min {
		if exact > 0 {
			return nil, fmt.Errorf("%q must decode to %d bytes, %d given", name, exact, len(data))
		}
		return nil, fmt.Errorf("%q must decode to at least %d bytes, %d given", name, min, len(data))
	}
	return data, nil
}

// Encrypt seals the secret with the password under the salt,
// drawing only the nonce, and spells the file's whole content:
// one JSON line closed by a newline. The name and the file
// are the caller's.
func Encrypt(secret, password string, salt []byte) ([]byte, error) {
	if len(salt) != saltSize {
		return nil, fmt.Errorf("salt must be %d bytes, %d given", saltSize, len(salt))
	}
	key, err := deriveKey(password, salt)
	if err != nil {
		return nil, err
	}
	var nonce [nonceSize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	box := secretbox.Seal(nil, []byte(secret), &nonce, &key)
	salt64 := base64.StdEncoding.EncodeToString(salt)
	nonce64 := base64.StdEncoding.EncodeToString(nonce[:])
	box64 := base64.StdEncoding.EncodeToString(box)
	var file bytes.Buffer
	if err := json.NewEncoder(&file).Encode(sealed{Salt: &salt64, Nonce: &nonce64, Box: &box64}); err != nil {
		return nil, err
	}
	return file.Bytes(), nil
}

// Open opens every secret with the password, the derived key
// memoized by salt: a directory sharing one salt pays the scrypt
// cost once. The first failure errors naming its secret - the
// wrong password answers with the first secret the scan read.
func Open(sealed []Secret, password string) (map[string]string, error) {
	opened := make(map[string]string, len(sealed))
	keys := make(map[string]*[keySize]byte)
	for _, secret := range sealed {
		key, shared := keys[string(secret.Salt)]
		if !shared {
			derived, err := deriveKey(password, secret.Salt)
			if err != nil {
				return nil, err
			}
			key = &derived
			keys[string(secret.Salt)] = key
		}
		var nonce [nonceSize]byte
		copy(nonce[:], secret.Nonce)
		plain, ok := secretbox.Open(nil, secret.Box, &nonce, key)
		if !ok {
			return nil, fmt.Errorf("secret %q: cannot decrypt", secret.Name)
		}
		opened[secret.Name] = string(plain)
		clear(plain)
	}
	return opened, nil
}

// deriveKey stretches the password against the salt into the
// sealing key.
func deriveKey(password string, salt []byte) ([keySize]byte, error) {
	var key [keySize]byte
	derived, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, keySize)
	if err != nil {
		return key, err
	}
	copy(key[:], derived)
	clear(derived)
	return key, nil
}
