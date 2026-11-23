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
	"fmt"
	"strings"
)

// Options is the parsed form of the command line.
type Options struct {
	// Indent is the per-level padding of the expanded log records;
	// empty keeps the one-line form. Set by --indent, which fixes
	// the padding at four spaces.
	Indent string

	// LogAt records snippet call positions in the requests' log
	// objects and error messages. Set by --log-at.
	LogAt bool

	// AddTLS names the tls pairs to generate under the tls
	// directory, then exit. Set by --add-tls, a comma-separated
	// list.
	AddTLS []string
}

// HelpRequested is returned when the user asks for the usage text.
// It is not an error: the caller prints the usage and exits cleanly.
type HelpRequested struct{}

func (e *HelpRequested) Error() string { return "help requested" }

// Usage spells out the command line surface.
const Usage = `Usage: rebear [options]

Rebear is a proxy that sits between local clients and the external
network, oriented towards the observation and modification of requests
and responses via Starlark scripts inlined into the configuration.

The setup is read from $REBEAR_HOME/rebear.cnf, or from
$XDG_CONFIG_HOME/rebear/rebear.cnf.

Options:
  --indent                Indent log records as multi-line JSON.
  --log-at                Record snippet position in logs and errors.
  --add-tls <name>[,...]  Generate the named certificates.

The certificate names provided to --add-tls are the same names used
in the configuration, and are stored under $REBEAR_HOME/tls/
`

// Parse resolves the argument list into Options. The --indent and
// --log-at flags are boolean. Nothing positional is accepted.
func Parse(argv []string) (*Options, error) {
	opts := &Options{}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "-h" || arg == "--help":
			return nil, &HelpRequested{}
		case arg == "--indent":
			opts.Indent = "    "
		case arg == "--log-at":
			opts.LogAt = true
		case strings.HasPrefix(arg, "--add-tls="):
			names := strings.Split(strings.TrimPrefix(arg, "--add-tls="), ",")
			opts.AddTLS = append(opts.AddTLS, names...)
		case arg == "--add-tls":
			i++
			if i >= len(argv) {
				return nil, fmt.Errorf("--add-tls needs a name list")
			}
			opts.AddTLS = append(opts.AddTLS, strings.Split(argv[i], ",")...)
		case strings.HasPrefix(arg, "-"):
			return nil, fmt.Errorf("unknown flag %q (see -h)", arg)
		default:
			return nil, fmt.Errorf("unexpected argument %q (see -h)", arg)
		}
	}
	return opts, nil
}
