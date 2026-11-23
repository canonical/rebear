# Preface

The notes feature: the global notes map the closing-flow
snippets share - the lock window, the canonical JSON state,
the day files under notes/, and the flush points.

IMPORTANT: Read the `.kb/agents.md` file.


# Concepts

## The map

A `closing-flow` snippet takes a fourth argument, `notes`: the
global notes map, shared by every flow and every generation.
It is a dict for all purposes - index, assign, `len`, `in`,
iteration, and the dict methods (`get`, `pop`, `update`, ...)
alike - but only the closings see it; the other snippet kinds
do not take it.

## The lock window

The canonical state is one JSON string, held by the store. A
snippet's first use of `notes` acquires the store - the string
travels out - and decodes it into the value's own dict; the
snippet's end encodes the dict back and releases the store with
the new string. Concurrent snippets serialize on the acquire:
while one holds the notes, no other flow's snippet can change
or observe them. A closing that never touches `notes` acquires
nothing. The store is blind to the content - it only guarantees
the custody of the string.

The release is the containment: a value the state cannot hold -
a function, say - fails the encode, logs on the culprit's flow
entry, and loses that snippet's changes alone - the store gets
the original string back, so the state stays, and the next
snippet decodes from it untouched. A state that does not decode
logs on the witnessing flow's entry and answers an empty map.

## The day files

The state flushes to `$REBEAR_HOME/notes/YYYYMMDD.json`, else
`$XDG_CONFIG_HOME/rebear/notes/YYYYMMDD.json` - resolved in
`cli.Run` beside the config path. The files are cumulative views of the
whole map, one per day, not the intra-day changes: a new day's
first flush writes the full state under the new name.

The flush runs every 60 seconds when the state changed since
the last write, and on SIGHUP, SIGINT, SIGUSR1, and the
context end. The write is atomic - a `.partial` file renamed
into place - and never fails a request: failures land on the
log stream.

## The start rule

`notes.New` (called from `cli.Run`) reads the latest day
file as the current state. A file up to one day into the
future is used - timezone shifts stay survivable - and the
flush then cannot write a file older than it, so the state
never slides onto a name a later start would ignore. Further
ahead the start fails: `notes file is in the future: <path>`.

## The handover

The store is global and owned by the caller: `cli.Run`
creates it once, hands the same `ProxyOptions.Notes` to every
generation - reloads included, no re-reading of the day files -
and flushes and closes it at exit. The draining generation and
the new one share one lock, one state, one flusher.
