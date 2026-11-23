# Preface

The tls settings: `tls-certs`, `tls-auth`, and `flags` on
inlets and outlets, the certificate pairs they name under the
TLS directory, and what each side does with them. `internal/setup`
parses the settings; `internal/tls` loads, generates, and
builds the Go configurations; `internal/proxy` serves and dials
with them.

IMPORTANT: Read the `.kb/agents.md` file.


# Concepts

## The two settings

Both settings are lists of names, on both kinds:

    inlets.local:
        tls-certs: ["foo"]             # server identities offered
        tls-auth: ["bar", "baz"]       # client certs accepted

    outlets.up:
        tls-certs: ["ca1", "*"]        # server certs trusted
        tls-auth: ["qux"]              # client identities presented

One name is one pair of files under the TLS directory:
`tls/<name>.cert` and `tls/<name>.key`. The directory resolves
like snapshots and notes: `$REBEAR_HOME/tls`, else the user
config dir's `rebear/tls`. A name follows the sniplet name
rule - the same one inlet and outlet names match. The `*` of
an outlet's `tls-certs` is not a name: parse pulls it out
into `TLSDefaultCerts`, so nothing downstream sees it.

One setting, one job, on both sides: `tls-certs` is about the
certificates of the endpoints - the identities an inlet offers
or an outlet trusts - and `tls-auth` is about the certificates
of the peers - the client certs an inlet pins or an outlet
presents. An inlet's `tls-auth` requires its `tls-certs`: pins
ride on a TLS listener.

A name prefixed with `+` may generate when its files are
missing; a plain name loads only, and a missing one is a
build error, `tls "name": missing certificate`. The prefix
is trimmed at parse and the prefixed names land in the
`TLSPlus` field, so nothing downstream sees the `+`.

The same generation is available ahead of time from the
command line: `rebear --add-tls foo,bar` writes the named
pairs under the tls directory and exits without serving -
an existing name or a bad one is refused. The generated
leaf carries the pair's name as CN and SAN, the hint of the
service pairs - an inlet's server pair takes the endpoint's
name instead, per addresses.md.

## The inlet server certificates

`tls-certs` on an inlet turns its listener into a TLS one. The
list is "I am any of these identities, your pick": the client
hello's server name picks the first leaf carrying that DNS
name, and a client that sends no SNI gets the first entry -
the list's own file order, the only default there is. Multiple
names spell rollover; SANs inside one cert are the better
spelling for aliases.

The server identity belongs to the socket, not to any one
inlet: inlets sharing an address must carry identical
`tls-certs` lists - a mismatch or a tls-plain mix is refused at
build. SNI is never a routing input: it is client-optional by
nature (an IP url sends none), and it only picks inside the one
agreed list.

## The inlet client pins

`tls-auth` on an inlet is a visibility filter, not a selector:
a client presenting none of the listed certificates cannot see
the inlet at all, and the routing among the visible inlets is
the usual vhost-then-mount walk, unchanged. The pins are exact
leaves - a SHA-256 of the leaf's raw DER, no chain semantics:
a cert signed by something in the list is not accepted, only
the listed certs themselves.

The handshake only asks: `ClientAuth` is `RequestClientCert`,
no `ClientCAs`, no verification by the TLS stack - the check
runs in the handler walk. An audience of several certs on one
route is several pins on one inlet, so the duplicate check -
same vhost and mount on one address - stands unchanged, pins
or no pins. A client matching no pin sees the 404
`client certificate is unknown`, and one that presents
no certificate sees `client certificate is missing`. The
matched pin's name rides every request record of the
client under `tls-auth`.

## The outlet side

`tls-certs` on an outlet is the set of server certificates
trusted from the remote: verification passes if the presented
chain reaches any of them, and the pool replaces the system
roots - `TLSDefaultCerts`, from the `"*"` name, adds them
back, a merge not a replacement. `tls-auth` on an outlet is what
the proxy presents; the handshake offers the first pair the
remote's signature preferences accept, so a list spells rollover,
but an outlet the remote ACLs by identity should keep one entry.
No url names the conversation then: the endpoint's name carries
the SNI and the verification, `localhost` for a socket - see
addresses.md.

## Flags

`flags` is a list of optional behaviors on any sniplet, spelled
with the sniplet name rule. One switch in `setup.checkFlags`
holds each flag's kind rule and its context check: unknown
flags, duplicate flags, flags on the wrong kind, and flags
without their context are all refused at load - a typo can
never silently do nothing. `trust-certs` is outlet-only and
needs a `tls-certs` list of pinned names: `TLSDefaultCerts` has
no leaf to pin, so it is refused with the flag.

    outlets.up:
        flags: ["trust-certs"]
        tls-certs: ["ca1"]

It switches `tls-certs` from trust anchors to exact leaf pins.
`InsecureSkipVerify` skips the whole `x509.Verify` call - chain,
root anchoring, expiry, key usage, name constraints, and the
host name - and `VerifyConnection` hashes the presented leaf's
raw DER against the pin set instead, on every connection
including resumptions. The address and the cert's SANs become
irrelevant; the pin is the whole trust decision, so an expired
or wrong-EKU pinned leaf still passes, and any cert change breaks
the dial until the list is updated. A redirect to an unpinned
host fails too. Without the flag, none of this applies: the pool
verifies the chain and the host name as usual. This is the
same `CertSet` the inlet's `tls-auth` uses, on the other side.

`unsafe-tls` is outlet-only too, and skips verification
altogether: no chain, no host name, no pins. It needs no tls
settings - `HasTLS` counts it, so the outlet speaks https and
refuses an `http` url. The `tls-certs` names still load, the
`*` is welcome, and the two flags cannot be used together: the pins
would still gate the handshake under the skip.

## Generation

A pair named with the `+` prefix - anywhere a name needs one -
is generated at proxy build time, in `New`, never in `Serve`: a
failure aborts the build and the reload semantics give the
"changes nothing" behavior. The pair is ECDSA P-256, self-signed,
ten years, key `0600`, cert `0644`, the CN and SAN from the
endpoint's name - see addresses.md - which turns an IP literal
into an IP SAN, not a DNS name: Go verifies IPs only against
`IPAddresses`.
Existing files are never overwritten: a regeneration would
silently break every pinned peer. A cert without its key loads
leaf-only - the anchor and pin uses never touch the key - and a
key without its cert is an error.

## The url scheme agreement

TLS is spelled by the settings, not by the url, but the two
must agree. An inlet with tls settings implies https and
refuses an `http://` url; an inlet without them refuses an
`https://` one. An outlet with tls settings or the `unsafe-tls`
flag refuses an `http://` url, and wherever the url names no
scheme - a
path-only one, or none at all - https applies;
without them both schemes stay open. The inlet default port
is 443 under tls, 80 without. See addresses.md
for the url forms.

## Where the code sits

`internal/tls` is the only package touching `crypto/tls` and
`crypto/x509`: a `Store` memoizing the named pairs,
`ServerConfig` for a shared listener, `ClientConfig` for an
outlet transport, `CertSet` for the exact pins on both sides.
The proxy wraps
the serving copy with the listener wrap - the master and the
reload handover are untouched, so a tls change rides a reused
listener like any url change. `ProxyOptions.TLSPath` names the
directory; empty disables the settings - a setup using them
against an empty path is a build error.
