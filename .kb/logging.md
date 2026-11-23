# Preface

The JSONL log stream: the line kinds, their keys, and how the
proxy fills one request record. `internal/logging` owns the
stream; `internal/proxy` and `internal/snippets` only feed it.

IMPORTANT: Read the .kb/agents.md file.


# Concepts

## One entry per line

`logging.Entry` is one line. The struct's field order is the
JSON key order, so the start record and the final one align
on screen: `time` first, then `method` and `path` and `query` and `flow`
and `tls-auth` - all a start record holds - with the
`out-method`, `out-path`, or `out-query` between their
plain forms and `flow` when the request was rewritten.
Then the
`out-*` timings of the request going out, the response's
`code`, the `in-*` timings of the response coming back, the
`log` list, and `error` last - the failure, when there is
one, is what the record ends on. Everything but `time` is
`omitempty`.

The marshal is the plain one, `internal/plainjson`: a string
escapes only what json itself requires, so no Go html escape
(`\u0026` and kin) ever lands in a line. A query thus holds
its wire form verbatim - the `&` raw, the `%26` raw - and a
newline inside an error or a logged message stays escaped, so
a line stays one line. The snapshot dumps, the bodies a
snippet re-encoded, and the fail bodies ride the same form:
every json rebear writes is plain.

The in/out naming follows the request's travel: it goes out
to the outlet first, then the response comes back in towards
the inlet. So the out-* keys describe the outgoing request,
the in-* keys the incoming response - out listed first, since
it happens first.

## The request record

    {"time": "...", "method": "POST", "path": "/v1",
     "flow": ["inlet", "outlet"], "out-ms": 123,
     "out-size": 4096, "code": 200, "in-ms": 45,
     "in-size": 1024, "error": "..."}

- `time` - the request's start, not its completion, in the
  stamp form `YYYYMMDD-HHMMSS-mss` - `logging.StampString`,
  the snapshot filenames' own, so a log line's time names its
  snapshot family exactly.
- `method`, `path` - the client's request, from the start
  record on. Both methods and both paths are outgoing really
  - the request's own and the outlet's - so the client's
  keeps the plain name and only the diverging one gets a
  prefix.
- `out-method` - the method sent to the outlet, appended only
  when a snippet rewrote it (`Entry.SetOutMethod` compares
  against `method`).
- `out-path` - the path sent to the outlet, appended only
  when the mount cut, the outlet url's path, or a snippet's
  assignment diverged from the client's
  (`Entry.SetOutPath` compares against `path`).
- `query` - the client's query from the start record on,
  capped at 64 characters - cut at 61 with a `...` tail
  beyond, so a log line stays short. The capping lands at
  write time, on `Write`'s own copy: the record keeps the
  exact form the divergence check needs.
- `out-query` - the query sent to the outlet, appended only
  when a snippet mutated it (the untouched one rides the
  wire verbatim, so it cannot diverge), capped the same.
- `flow` - the route handling the request: the inlet from the
  start record on, the midlets and the outlet joining once
  they settle. On a shared listener, a request no url fits -
  wrong host, or wrong path - is a 404 no inlet can be named
  for: the record carries no flow at all - and it is the
  request's only record, written once at the end since
  nothing ran to trace.
- `tls-auth` - the name the client's certificate pinned to,
  from the start record on, joined only when the inlet
  pins and the client matched: a certless or
  unrecognized client carries no key.
- `out-ms`, `out-size` - the outgoing request's span (start to
  outlet send) and the body size leaving for the outlet.
  Absent when the request never reached an outlet. The span
  rounds up to whole milliseconds: a send under 1 ms shows 1,
  not a key vanished under omitempty.
- `code` - the HTTP status the client saw: the response's own
  code, a snippet's rewrite of it, or the locally generated
  one on an error path.
- `in-code` - the code that came in from the outlet, appended
  only when a snippet rewrote it away
  (`Entry.SetInCode` compares against `code`).
- `in-ms`, `in-size` - the incoming response's span (outlet
  send to client write-back) and the body size written back.
  Absent when no outlet response arrived. The span rounds up
  like the outgoing one.
- `log` - one object per `log()` or `print()` call from a
  snippet, appended in call order. A `log()` object carries
  the call's `at` position - the snippet's config path and
  line, no column - beside its keys, but only while
  positions are recorded: the CLI's `--log-at` flag turns
  that on, off by default. A snippet's own `at` key is
  overwritten, keeping the position honest. print is sugar:
  `print("foo", "bar")` lands as `{"print": "foo bar"}`,
  and its `at` is always recorded, `--log-at` or not.
- `error` - the failure message, the fail() code included. The
  snippet's position joins it - `at:line:col: message`, no
  function name - only while positions are recorded: the CLI's
  `--log-at` flag, off by default.

The handler opens with `logging.Begin(method, path)`, sets
`Flow` to the inlet, and writes the entry twice: once right
away - a start record holding the inlet, method and path, so
a slow request leaves its trace from the first line - and
once in a deferred `logging.Write(*entry)` - deferred as a
closure, so the copy is taken at return time, after the
request filled the entry in. The outlet settling later
replaces `Flow` with the full route. Two records per request,
always: successes, snippet failures, and the `http.Error`
paths that stayed silent before. The two markers bracket the
forwarding: `Sent(size)` right before the outlet request goes
out, `Done(size)` after the response is written back. Both
derive from moments recorded on the entry, never marshaled.

## The other line kinds

    {"time": "...", "log": [{"msg": "started listening", "inlet": "local", "address": ".../sock"}]}
    {"time": "...", "log": [{"msg": "from inlet"}]}

Both non-request lines share the stray-line form: a bare `log`
list, the same field a request record's log() calls use, one
message from outside any request. `logging.Log` is their
producer: the message lands under `msg` (callers format their
own), and any key/value pairs land beside it - so
`Log("paused", "seconds", 3)` emits
`{"log": [{"msg": "paused", "seconds": 3}]}`. Not to be
confused with the entry method of the same name, which
appends to a request record.

The started-listening line reports one inlet listening, under
an `inlet` key beside the message, with its `address`. One
line per inlet, so the inlets sharing a listener each report
it. The reload writes three messages:
`{"msg": "SIGHUP - reloading configuration"}` right when the
signal is acknowledged - before the work starts, so the line
lands even when the reload is instant - then
`{"msg": "SIGHUP - configuration reloaded"}` on a successful
swap, or `{"msg": "SIGHUP - cannot reload configuration",
"error": "..."}` when the new setup fails to load or build:
the current one keeps serving either way.

The http server's own complaints land on the entry's own
error field, with a log object of their own: the `ErrorLog`
handed to each listener turns every internal `net/http` line -
a TLS handshake that died before a request existed, accept
trouble - into an entry holding the message under `error` and
one log object with the listener's inlets and address. The raw
log package's stderr line is gone.

## The sink

`logging.SetWriter(io.Writer)` sets the stream's sink,
mutex-guarded. The CLI points it at Stdout. A nil writer
drops the output; an unset writer in tests is the same.

`logging.SetIndent(padding)` switches every record to the
multi-line form `json.Indent` produces, one blank-free object
per record; empty restores the one-line form. The CLI wires
it to the boolean `--indent` flag, which fixes the padding at
four spaces. Call it before the stream starts.
