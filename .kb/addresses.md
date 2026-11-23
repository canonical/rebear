# Preface

How rebear listens and connects: the `address` setting that
names the net endpoint, and the `url` setting that routes - the
inlet's mount and vhost, the outlet's path prefix. The two
together spell the endpoint's name - the identity a
conversation or a certificate speaks. `internal/setup` parses
them; `internal/proxy` binds, dials, and builds the outgoing
URLs.

IMPORTANT: Read the .kb/agents.md file.


# Concepts

## Two settings, two jobs

Every inlet and outlet takes the same pair:

    inlets.local:
        address: "/run/rebear.sock"        # or "127.0.0.1:8080"
        url: "http://api.example.com/v1"   # or "/v1", or unset

    outlets.up:
        address: "10.0.0.5:8443"           # or "/run/upstream.sock"
        url: "https://api.example.com/api" # or "/api", or unset

The `address` is the net endpoint: where the ear goes (inlet) or
the dial lands (outlet). An address starting with `/` or `./` is
a unix socket path, resolved to absolute against the cnf file's
directory at load; anything else is a tcp `host[:port]`, kept
except for the port, which is materialized at load: 80 for an
inlet, or 443 when it carries tls settings, and for an outlet
the default port of the scheme it speaks - see the outlet url
section. An address with `://` is refused as a url that
belongs in the url setting, and one with a slash anywhere else
is refused as a socket path missing its lead. The address is
never parsed for routing; it names the endpoint instead - see
the endpoint's name.

The address may be omitted when the url names a host: it then
defaults to the url's own `host[:port]`, normalized like a
spelled one. No address and nothing to take one from - no url,
or a path-only one - is `missing address` either way. An
explicit address always wins.

So the stored address names the socket one way everywhere -
listener grouping, dial, reload handover - with no respelling
of the same socket left possible: `example.com` and
`example.com:80` are one listener, not a bind conflict. The
`url` is never rewritten by this: the port belongs to the
socket, not to the name the conversation speaks. Where the url
names no host, the address names the conversation instead -
see the endpoint's name - with the scheme's default port
elided.

The `url` routes, and names the endpoint when it spells a
host, in exactly three forms: a path starting with `/`, a full
URL with `://`, or unset. A bare `host/path` is refused. Only
what the config says is stored on the `*url.URL` - no implied
scheme, no host added to an unset one - so an empty host stays
distinguishable from a named one, the split the endpoint's
name rides on. An inlet's scheme is http, or https when the
inlet carries tls settings - there an http url is a conflict,
and an https one without them is. An outlet takes http or
https, an http url conflicting with its tls settings the same
way. See tls.md for the settings themselves.

## The endpoint's name

Every inlet and outlet carries one name - `Inlet.Host` and
`Outlet.Host`, resolved at use, never stored: the url's host
when it names one, else the host of a tcp address, else
`localhost`: a socket or an unspecified address names no host
of its own.

The name is the identity the endpoint speaks: the inlet hands
it to the generation of its server certificates - the leaf
carries it as CN and SAN - and the outlet's conversation
carries it as the Host header and the SNI of the outgoing
request. It feeds no routing, no dial, no bind. See tls.md
for what the pairs do with it.

A certificate never names a port - the generation drops it,
and the verification asks for the host alone - while the
conversation keeps the non-default ports: the url's own as
spelled, an address-named one eliding the scheme's default.
An unspecified address - `0.0.0.0`, `[::]` - names `localhost`:
only local clients verify its leaf, so an inlet serving the
network names its host in the url.

## The inlet url: vhost and mount

The inlet matches its url against the request in two parts. The
host, when the url has one, is matched against the request's
Host header exactly, port included - `api.example.com:8080` does
not match a bare `api.example.com` - vhost-style; a url without
a host matches every request. An IP host matches nothing: the
config may put it there for the address alone, and no request's
Host is an IP's name, so it constrains no inlet (`vhostOf`). The
bare name `localhost` - any port, any case - matches nothing for
the same reason: it names the address, never the request. The
path is the mount: matched to decide if the inlet is the right
one, then cut from the outgoing path before it is passed on the
flow. An unset url is no constraint at all: any host, any path.

## The outlet url: prefix, host, scheme

The outlet speaks one scheme: the url's own when it spells one,
else the implied one - https under any tls setting or the
unsafe-tls flag; http to a
local address, a unix socket or a loopback host whose spelled
port is not 443; https to everything else. The missing port of
a tcp address takes the scheme's default, 80 against 443, and
a spelled 443 on a local host selects https.

The outgoing request's scheme and host resolve at use time, in
`serve`, never at load:

1. a url with a host - the url's own scheme and host;
2. no url host - the outlet's scheme plus the endpoint's name:
   the address's host for a tcp outlet, without the scheme's
   default port, and `localhost` for a socket. The path of a
   path-only url still applies.

The url's path prefixes the trailing request path in every case.

## Address and url are independent

The dial takes the address, the request URL takes the url, so
the two may name the same service differently:
`address: "10.0.0.5:443"` with `url: "https://api.example.com/api"`
connects to the IP while sending Host and SNI `api.example.com` -
the curl `--resolve` pattern. Every outlet gets its own
`http.Client` whose transport dials the address instead of the
url's host, unix or tcp alike. Without a url naming a host,
the two collapse into one: the address is both the dial and
the conversation, per the endpoint's name.

## Sharing a listener

Inlets may share one address when their urls differ. The proxy
groups inlets by address before binding, and one listener and one
`http.Server` serve each group as a `proxy.sharedListener`
holding one handler per inlet. The duplicate key is the vhost
plus the mount: two inlets differing only in vhost are legal,
an exact repeat is refused at startup. The handlers sort
vhost-bearing first - a vhost is more specific than any mount -
then by descending mount length, and the serving walk takes the
first match: `http://api.example.com/api/v2` over the hostless
`/api` catch-all. `Parse` ranges over a map, so inlet order is
randomized and config order cannot be the tiebreak - and either
inlet may be named when a duplicate is reported.
