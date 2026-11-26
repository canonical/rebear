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
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/canonical/rebear/internal/secrets"
)

// An asker reads the sensitive lines of one run: passwords
// and secrets, hidden on a terminal, plain on a pipe, or
// from the keyring under --keyring.
type asker struct {
	// tty is stdin while it is a terminal; a nil tty reads
	// plain lines from the reader.
	tty *os.File
	// reader buffers the plain lines.
	reader *bufio.Reader
	// stderr takes the prompts and the alignment newlines.
	stderr io.Writer
	// keyringHome names the keyring entry the password asks
	// read, the --keyring choice; empty keeps the password
	// asks on stdin.
	keyringHome string
}

// newAsker builds the asker over the given streams. A set
// keyringHome moves the password asks to that keyring entry;
// an empty value keeps them on stdin.
func newAsker(stdin io.Reader, stderr io.Writer, keyringHome string) *asker {
	ask := &asker{reader: bufio.NewReader(stdin), stderr: stderr, keyringHome: keyringHome}
	if file, ok := stdin.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		ask.tty = file
	}
	return ask
}

// readLine reads one line from stdin. A terminal prints the
// prompt to stderr and hides the input, and ends the ask with
// a newline; a pipe reads plainly, with no prompt.
func (a *asker) readLine(prompt string) (string, error) {
	if a.tty == nil {
		line, err := a.reader.ReadString('\n')
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if err != nil {
			if err == io.EOF && line != "" {
				return line, nil
			}
			return "", err
		}
		return line, nil
	}

	fd := int(a.tty.Fd())
	fmt.Fprint(a.stderr, prompt)
	// The line discipline is restored inside ReadPassword when
	// the read returns; this state guards the other outcome.
	state, err := term.GetState(fd)
	if err != nil {
		return "", err
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
	defer signal.Stop(sigs)

	type read struct {
		data []byte
		err  error
	}
	done := make(chan read, 1)
	go func() {
		data, err := term.ReadPassword(fd)
		done <- read{data, err}
	}()
	select {
	case result := <-done:
		// Harmless on the normal path, decisive on the
		// signal one.
		_ = term.Restore(fd, state)
		fmt.Fprint(a.stderr, "\n")
		if result.err != nil {
			return "", result.err
		}
		line := string(result.data)
		clear(result.data)
		return line, nil
	case sig := <-sigs:
		// The discipline restore never ran: leave the
		// terminal as it was, put the default disposition
		// back, and raise it again.
		_ = term.Restore(fd, state)
		signal.Stop(sigs)
		signal.Reset(sig)
		syscall.Kill(os.Getpid(), sig.(syscall.Signal))
		select {}
	}
}

// readValue reads one sensitive line named by the noun: the
// prompt and the failed read spell the noun.
func (a *asker) readValue(noun string) (string, error) {
	value, err := a.readLine(noun + ": ")
	if err != nil {
		return "", fmt.Errorf("cannot read the %s: %v", noun, err)
	}
	return value, nil
}

// readAgain asks the value again on a terminal until the two
// reads agree, and answers the agreed value; a pipe accepts
// the value as read. A differing pair hides the wrong side,
// so both are read again, the notice spelling the plural and
// the failed ask the noun.
func (a *asker) readAgain(noun, plural, value string) (string, error) {
	if a.tty == nil {
		return value, nil
	}
	for {
		confirm, err := a.readLine("again: ")
		if err != nil {
			return "", fmt.Errorf("cannot read the %s again: %v", noun, err)
		}
		if confirm == value {
			return value, nil
		}
		fmt.Fprintf(a.stderr, "error: the two %s differ\n", plural)
		if value, err = a.readValue(noun); err != nil {
			return "", err
		}
	}
}

// readPassword reads one password: the keyring entry when
// --keyring chose it, else one typed line. Every ask prints
// the one prompt, the secrets password's.
func (a *asker) readPassword() (string, error) {
	if a.keyringHome != "" {
		return a.readKeyringPassword()
	}
	return a.readValue("secrets password")
}

// readSecret reads the secret and asks it again on a
// terminal, always: the keyring holds passwords only, so the
// secret is typed and confirmed under --keyring too.
func (a *asker) readSecret() (string, error) {
	secret, err := a.readValue("secret")
	if err != nil {
		return "", err
	}
	return a.readAgain("secret", "secrets", secret)
}

// verifyPassword makes sure the password opens the sealed
// secrets, reading it once more when they refuse - a wrong
// one is usually a slip of the fingers - and answers the
// password of the last try. The open failure is answered as
// `wrong password: ...`, the one spelling of the refusal,
// and a failed read as its own. A keyring entry and a
// pipe never read again: the entry is one lookup, and a
// pipe has one line per ask.
func (a *asker) verifyPassword(password string, sealed []secrets.Secret) (string, error) {
	if _, err := secrets.Open(sealed, password); err != nil {
		if a.tty == nil || a.keyringHome != "" {
			return "", fmt.Errorf("wrong password: %v", err)
		}
		fmt.Fprintf(a.stderr, "error: wrong password: %v\n", err)
		if password, err = a.readValue("secrets password"); err != nil {
			return "", err
		}
		if _, err = secrets.Open(sealed, password); err != nil {
			return "", fmt.Errorf("wrong password: %v", err)
		}
	}
	return password, nil
}

// checkPassword confirms the password and answers the one to
// keep: opening the sealed secrets is the confirmation, and
// a typed one that fails it is read again once, with the
// refusal spelled by verifyPassword. Without seals, the
// again ask settles the typed password - the agreed value
// wins, for nothing on disk can check it - and a keyring
// entry is taken as is.
func (a *asker) checkPassword(password string, sealed []secrets.Secret) (string, error) {
	if len(sealed) == 0 {
		if a.keyringHome == "" {
			return a.readAgain("secrets password", "passwords", password)
		}
		return password, nil
	}
	return a.verifyPassword(password, sealed)
}

// readKeyringPassword reads the password of the keyring entry
// the home names, through the secret-tool CLI. The tool names
// itself in its failures, and those messages stay verbatim.
// An empty answer refuses: no entry exists, or the entry holds
// the empty password, and the two cases cannot be told apart.
func (a *asker) readKeyringPassword() (string, error) {
	bin, err := exec.LookPath("secret-tool")
	if err != nil {
		return "", fmt.Errorf("--keyring: %v", err)
	}
	var stderr strings.Builder
	cmd := exec.Command(bin, "lookup", "application", "rebear", "REBEAR_HOME", a.keyringHome)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	password := strings.TrimSuffix(strings.TrimSuffix(string(out), "\n"), "\r")
	clear(out)
	if err != nil && stderr.Len() > 0 {
		return "", fmt.Errorf("--keyring: %s", strings.TrimRight(stderr.String(), "\n"))
	}
	if password == "" {
		return "", fmt.Errorf("--keyring: no rebear password for REBEAR_HOME %q", a.keyringHome)
	}
	return password, nil
}

// storeKeyringPassword stores the password in the keyring entry
// the home names, through the secret-tool CLI: the same
// attributes readKeyringPassword looks up. The password
// reaches the tool without a newline, the documented store
// spelling. The tool names itself in its failures, and those
// messages stay verbatim.
func storeKeyringPassword(home, password string) error {
	bin, err := exec.LookPath("secret-tool")
	if err != nil {
		return fmt.Errorf("--set-keyring: %v", err)
	}
	var stderr strings.Builder
	cmd := exec.Command(bin, "store", "--label=rebear", "application", "rebear", "REBEAR_HOME", home)
	cmd.Stdin = strings.NewReader(password)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			return fmt.Errorf("--set-keyring: %s", strings.TrimRight(stderr.String(), "\n"))
		}
		return fmt.Errorf("--set-keyring: cannot store the password: %v", err)
	}
	return nil
}
