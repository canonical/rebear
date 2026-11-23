# Preface

The flow design over the snippet runtime: one program
for the whole setup, the flow value the snippets see, the
stages a request moves through, and where the outlet choice
happens. This is the settled design behind `internal/snippets`
and the proxy's request path.

IMPORTANT: Read the .kb/agents.md file.


# Concepts

## One program for the whole setup

The proxy cannot know up front which outlet a request will
leave through: a snippet may reroute it. So the snippets of
the entire setup - all inlets, midlets, and outlets - assemble
into one starlark program, compiled once at startup. A program
with no snippets anywhere compiles to nil; its calls are no-ops.

A per-pair program cannot answer this: the pair is not known
until the request has been filtered, and the filters are the
thing being run.

## The name rule

Inlet, midlet, and outlet names must match

    ^[a-z][a-z0-9]*(-[a-z0-9]+)*$

The check runs in `snippets.Compile`, on every inlet, midlet,
and outlet, snippetless ones included: that is the point where
name becomes an identifier, so it is where a bad name is
refused - not in the config loader, which has no reason to
know the constraint.

The rule forbids `_` in names. That is what makes the function
names below collision-free by construction: no name can embed
the separator.

## The snippet table

Compile builds a map beside the assembled funcs:
sniplet => field => snippetFunc, by value. The snippetFunc
wraps the assembled Func itself, under the field name fnc - a distinct
type today (holding the one Func, Name included), so that
streaming JSON can grow it later: a single incoming- key
may then hold several funcs. Presence is a map lookup, and
the call path reads the def name from the map value - the
name is computed once, at Compile, and never rebuilt at
runtime. The program drives the flow through this table:
SetOutlet, the opening and closing cores, and every hook call
sit on Program, with the flow carrying plain data. The table
cannot resolve outlet names - a snippetless outlet sits
nowhere in it - so the program also carries a name-keyed
outlet registry for set_outlet.

## Function names

One def per snippet field, the entity's name first with `-` as
`_`, then the role (inlet or outlet), then the field:

    local_inlet_opening_flow
    remote_outlet_closing_flow
    remote_outlet_outgoing_headers
    remote_outlet_outgoing_json
    local_inlet_incoming_json
    strip_midlet_outgoing_headers

An inlet, a midlet, and an outlet may share a name: the role
word keeps their functions apart. The predeclared glue names
(`flow`, `outgoing`, and `incoming` are not ones - they are
arguments) cannot collide with these.

## The flow value and the message values

Snippets do not take a bare state dict. They take a `flow`
value, always beside two message values - `outgoing` and
`incoming` - handed to every snippet as arguments:

- `flow.state` - the flow's dict, the same reference on
  every access; what an inlet records, the midlets and the
  outlet and the response half read.
- `outgoing` - the outgoing request as it currently stands:
  `method`, `path` (the mount-cut trailing - the outlet's
  url path is not on it: no snippet knows the outlet, so the
  prefix joins at the send, from the settled one), `type` (the
  cleaned Content-Type - media type alone, parameters dropped
  - empty when unset or unparseable), `headers` - the live
  header dict, the same one the `outgoing-headers` stages
  receive, so a mutation is visible everywhere at once, and
  `query` - the URL's query as a dict-like object over the
  first value of each parameter (the `getn` family answers
  the whole values lists). Reading the query changes nothing:
  only a mutation marks it updated, handing the encoding to
  `url.Values` - until then the client's raw query rides the
  wire verbatim, awkward constructions included, the path's
  escaped-passthrough idea over again.
- `incoming` - the incoming response the same way, from its
  arrival to the client write-back: `code` (an int) and
  `type`, beside the same live `headers` dict on the
  `incoming-headers` side. Every field reads None before
  the response arrives.
- `flow.set_outlet(name)` - record the outlet the request
  will leave through, looked up among the setup's outlets.
  An unknown name is a starlark failure. So is a call once
  the outlet is running (see stages): routing happens while
  the choice is still open.
- `flow.add_midlet(name)` - append a midlet to the
  chain's queue, looked up among the setup's midlets. The
  inlet's config list and each settled midlet's own sit
  ahead of it, so the record runs after them. An unknown
  name is a starlark failure, as is a call once the outlet
  is running. A midlet already reached is not added: it is
  in the past (see the midlet chain).

Every `outgoing` and `incoming` field assigns - the
assignment replaces the value as it is currently best known.
An assigned path is still the trailing: the outlet's prefix
joins at the send, from the settled outlet. The `headers`
dicts never replace - mutate them in place, and the `query`
object mutates the same way, its dict methods and index
assignment included. The locking is the
stage lifecycle on the Flow: opening at birth, outgoing once
the inlet's opening-flow ran, incoming once the request left,
closing once the response reached the client, closed once the
closings ran - each phase running its own work (the opening
snippet inside outgoing, the closing ones inside closed) and
each step freezing what it ends (the dicts by starlark's own
freeze), so a write past a lock fails, and a flow never opens
or closes twice. The proxy seeds the flow before the opening
runs (`NewFlow`, `SetOut`), records the arrival with `SetIn`,
and locks at the delivery points.

The three are host types; `internal/scripts` stays
blind to them, as to every argument.

## The log builtin

Beside the flow value, every snippet sees `log(dict)`: it
appends the dict to the request's log record, as one object
under the record's `log` list. Repeated calls stack in order.
Values convert for JSON: strings, ints, floats, bools, `None`
to nil, lists and dicts recursively; anything else fails the
call, as does a non-dict argument.

The call's position joins the keys as `at` - the snippet's
config path and line, no column - only while positions are
recorded: off by default, on via the CLI's `--log-at` flag,
which reaches the flows through `ProxyOptions.LogAt` and
`Program.SetLogAt`, the program stamping every flow it
starts through `Flow.SetLogAt`, which forwards to the
script state too. A snippet's own `at` key is overwritten,
keeping the position honest.

Errors ride the same flag: a snippet failure in a request
record renders its message alone by default, and gains the
innermost call site's `at:line:col:` prefix - function names
dropped - once positions are recorded.

print is the same channel: `print("foo", "bar")` lands as
`log({"print": "foo bar"})` would, message joined by spaces.
A print's `at` is always recorded, `--log-at` or not.

The plumbing: the flow rides the thread as a local for
each call (`State.SetLocal`), carrying the request's log
entry, and the builtins read it back through their thread
argument, with `State.At()` for the position.

## Stages and the outlet opening

A request moves through stages, and each stage runs known
entities' snippets:

1. Flow starts; the inlet's `opening-flow` runs.
2. The inlet's `outgoing-headers` runs. `set_outlet` and
   `add_midlet` work.
3. The body decision (below). If the body is buffered, the
   inlet's `outgoing-body` runs - `set_outlet` still works:
   routing on the body is allowed.
4. The midlet chain: each queued midlet settles and runs its
   `opening-flow`, then its `outgoing-headers` and
   `outgoing-body`. The inlet's `midlets` list seeds the
   queue, each settled midlet's own list joins behind the
   ones already queued, and `add_midlet` appends from the
   snippets.
5. The outlet is settled - `set_outlet`'s record, else the
   config's outlet field - and its `opening-flow` runs. Nothing
   is chosen by name match: with no field on the inlet or a
   midlet and no `set_outlet` call, the request fails with
   502, `no outlet selected`. From here the outlet is
   running, and both `set_outlet` and `add_midlet` fail:
   nothing after the outlet's snippets could act on a
   further choice.
6. The outlet's `outgoing-headers` runs, then its
   `outgoing-body` on the buffered body.
7. The request leaves through the settled outlet's address,
   its url naming the request (see addresses.md); the
   response walks the incoming stages back: the
   outlet's filters, the midlets' in reverse order, then the
   inlet's.
8. The flow closes: the `closing-flow` snippets of every
   sniplet that opened without failure run in reverse
   opening order - the outlet, the midlets in reverse, the
   inlet - once the response is out or the flow has failed.
   They take a fourth argument, `notes`, the global notes map
   (see notes.md). The closings ride the flow's log entry; a
   closing failure lands there as an error object and the
   remaining closings still run, since nothing is left to
   answer.

The outlet opening has one moment, stage 5, regardless of how
the outlet was chosen: there is no separate early opening for
a `set_outlet` reroute.

## The midlet chain

A midlet is a sniplet that neither listens nor connects: it
sits between the inlet and the outlet, filters the request,
and may override the outlet choice. The inlet names its
midlets with its `midlets:` list, in order; each midlet may
name the ones after it with its own. Any number may run, and
none is required.

The chain is a queue. The inlet's config list seeds it before
its snippets run; a settled midlet's own list joins behind
everything already queued - config first, `add_midlet`
records behind. A midlet runs on its first listing alone:
every later listing of a reached name is skipped, silently,
and an `add_midlet` naming one is a no-op. No loop is
refused or possible - the queue drains, each name once.

Both routing choices are recorded values that stay open while
the chain runs, so precedence falls out of the recording
order: a later midlet's `outlet` field or `set_outlet` call
overwrites an earlier record, and any midlet's overwrites the
inlet's. A midlet's script beats its own static field, the
same as an inlet's does. The outlet settles last, from the
final record.

A snippetless midlet is legal: it exists purely to route. It
lives in the name-keyed registry on the program, not in the
snippet table, like a snippetless outlet.

## The body decision

The body is buffered lazily: only at the first sniplet whose
`outgoing-body` runs, and only for that reason. A flow that
reaches the outgoing request without a single body-filtering
sniplet along it streams the body untouched - that flow was
non-buffering by definition, so nothing is read and no memory
is held.

The read is safe to postpone because nothing touches the body
before the outgoing request is built: there is nothing to send
it to before the flow is discovered. Within every sniplet the
stage order is unchanged - `opening-flow`,
`outgoing-headers`, then `outgoing-body` - so the bytes are
read at the first stage that has one, never before.

The memory limit moves with the buffering: the first read of
an oversized body fails the flow at that stage, whatever
snippet would have run on it. A body buffered at an early
stage but crossing no further filtering still leaves through
the buffered bytes, from memory, unchanged.

The incoming side has no such problem: the outlet is settled
and running by the time the response returns, and the midlets
that ran are known, so the test is exact - the settled outlet
has an `incoming-body`, or one of those midlets does, or the
inlet has one.

The incoming side splits on the content type: a
text/event-stream response with a route holding an
`incoming-event` streams, everything else buffers
through the `incoming-body` lane - a body filter on a
stream response buffers the whole stream as one body.
The split reads the headers as the `incoming-headers`
stages left them, so a snippet flipping the content type
switches the lane.
Each event's `data` lines join
with `\n` into one payload, the route's `incoming-event`
snippets run once per event on it - the outlet, the midlets in
reverse, the inlet - and the event flows to the client as it
arrives, flushed. The snippet takes an `event` argument - the
same bodyValue Go type, its surface keyed on the event
pointer: `name` (reading "message", the spec's default, when
the event carries no event line, without writing it back),
`data`, `json`, `id` (strings), and `retry` (an int, the
milliseconds), all assigning - empty string, or None for
retry, drops the field. An event the chain never touched - the
value still raw, the fields unchanged - crosses verbatim, byte
for byte; a touched one is rewritten, the data split back into
`data:` lines. The headers go out before the first event, so a
snippet failure or an over-limit payload mid-stream cuts the
stream instead of answering a status. A route holding
neither filter never parses the stream: a pure copy, event
stream or not.

A buffered body travels as one `snippets.JSONValue` (snippets
package): the raw bytes until the first `outgoing-body` or
`incoming-body` asks for the value, then a single starlark
value that every snippet in the chain works on in place -
handed to the snippets as the `json` field of their `body`
argument (a `bodyValue` wrapping the JSONValue), so a
dict or a list mutates, the scalar kinds (string, number,
bool, null) arrive to observe. The `data` field is the raw
bytes, re-encoded once when the value took over. Assignment
replaces - `body.json = v` swaps the value, `body.data = s`
the bytes, kind re-sensed - feeding the next stage; a
snippet's return value is ignored. Assigning `None`
writes a JSON null - a null body reads back as `None`, so
the identity assignment round-trips. `Kind()` reports the kind the
first rune promises, without guaranteeing the value is valid.
A body that is not JSON at all has no decoded form: `body.json`
reads as `None`, and the original bytes pass through. The
typed accessors (`Dict()`, `List()`,
`String()`, `Number()`, `Bool()`, `Null()`) answer
`ErrValueType` on a kind mismatch, also unstored. Genuine
failures - a decode over JSONLimit, an unencodable value -
are stored on the value and answered from then on (`Err()`
reports them), failing the flow at the stage that tripped
them.

## Positions and print

Each assembled def carries its config path (`At`), like
`inlets.local.outgoing-headers`. Error and print positions
report in the author's snippet: lines count from the
snippet's first line, columns discount the baked indent -
written as `inlets.local.outgoing-headers:2:5`.

Print reports through the same path: the print handler finds
the call site on the thread's stack, maps it back, and hands
the caller the thread, the position, and the message apart
(`State.At()` exposes the same walk). The snippets wiring
lands the message on the flow's log entry as
`{"print": msg, "at": line}`; without a flow behind the
call it becomes a bare log line through `logging.Log`.
