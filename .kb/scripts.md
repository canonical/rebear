# Preface

The starlark snippet runtime for rebear: how snippets from the
setup assemble into scripts, how flows run them, and the
decisions behind the current `internal/scripts` package.

IMPORTANT: Read the .kb/agents.md file.


# Concepts

## Why an assembled file

Snippets do not run as REPL chunks. Each inlet+outlet pair's
snippets assemble into a single starlark file - one `def` per
filter, plus host glue - which is compiled once at startup
(`scripts.New`). A snippet that fails to parse, resolve, or run
at toplevel surfaces there, at startup, not mid-request. The
assembled file wins on:

- compile errors arrive at startup with file/position,
- predeclared kwargs is the native data path, no staging dance,
- the file is honest output: dumpable, debuggable, portable.

## Frozen shared globals

The file's toplevel runs once, in `scripts.New`, and the
resulting globals are frozen and stored on the `Script`; every
flow shares them.

Freezing is transitive: any value the toplevel built - including
containers in the glue (`GLOBAL = {}`) - freezes too. A function
mutating one fails at the call (`cannot insert into frozen
hash table`).

Why freeze at all: frozen data is safe to share across flows
with no locking, and it forces all mutable data through the call
arguments, making each filter's dataflow explicit.

## State as an argument

What freezing blocks is flow-shared mutable state - a request
filter recording something for the response filter. The fix is
a value passed as an argument to every function.

- The package itself is blind to it: it is simply a kwargs
entry like any other. The convention lives in the wiring.
- Readback comes from holding the same reference: a dict passed
in kwargs is the object the function mutates.
- In rebear the values are the `flow` object plus the
`outgoing` and `incoming` message values (see flows.md): the
flow's `state` dict and routing, the two messages' facts.

## Calls and kwargs

`State.Call(name, pairs...)` lays its name/value pairs out as
starlark keyword arguments. Starlark itself polices the match:

- unknown key: `unexpected keyword argument "k"`,
- missing key: `function f missing 1 argument (k)`.

Both arrive wrapped as errors carrying the starlark backtrace.
The function's return value is passed along; filters that mutate
dict arguments in place do not need to return.

## The init split

Two different inits exist and must not be confused:

- setup's `opening-flow:` config field - the user's snippet,
  wrapped by the wiring into the `opening-flow(flow)` function
  body (beside its `closing-flow(flow)` counterpart),
- `Options.Init []string` - host glue written at the end of the
  assembled file, at column 1, run by toplevel. Constants only.

## Naming

`Options.Name` names the assembled file; empty defaults to
`rebear`. It survives in the thread name, the wrapper's own call
errors (`pair: unknown function "x"`), and as the fallback for
positions whose func set no At. The snippets wiring leaves it
default: entity positions carry At instead.

## Position remapping

Assembled bodies sit after an invisible 4-space indent, but
error positions surface to the author. Each func carries an
`At` - the config path of its snippet, like
`inlets.local.outgoing-headers`. All `name:l:c` positions in
compile errors and backtraces are rewritten inside a func
body to `At:l':c'`: the line counted from the snippet's first
(`l' = l - first + 1`), the column dropping the body indent and
gaining the func's `Indent` - the snippet's column offset in
its original source. Positions outside any body (def headers,
glue) pass through unchanged.

Print hands the position to the caller: the print handler walks
the call stack - builtin frames report `<builtin>` and are
skipped - and passes the innermost call site, remapped
through the same spans, to the `Print` callback beside the
message. The callback decides what to do with both. The
frame position is the call's open paren.

`State.Call` renders an eval error as its message alone -
`EvalError.Msg`, the backtrace never built. A state recording
positions (`State.SetLogAt`) prefixes the innermost frame's
remapped position instead, the function name dropped. Startup
errors - compile and toplevel - always carry the position.
