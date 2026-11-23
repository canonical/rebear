# Preface

The snapshot feature: how snippets dump payloads onto files on
disk - the starlark calls, the filename grammar, the tee points,
and the log file that rides beside every dump.

IMPORTANT: Read the `.kb/agents.md` file.


# Concepts

## The calls

`outgoing.snapshot(name)` and `incoming.snapshot(name)` are
methods on the two message values. `outgoing.snapshot` is legal
from any snippet until the request leaves (`LockOut`);
`incoming.snapshot` from any snippet until the response is
written back (`LockIn`) - before the response arrives included.
Past the lock the call fails: `cannot snapshot outgoing: request
already sent`. The name follows the sniplet name rule - it lands
in a filename - and a bad name fails the call.

The flow records the names per kind, deduplicated (`SnapshotOut`,
`SnapshotIn`, `OutSnapshots`, `InSnapshots`, `SnapshotNames`): one name may
ride both kinds, and the log file writes once per distinct name.

## The filename grammar

    <name>-<YYYYMMDD>-<HHMMSS>-<ms>-<out|in>.<json|txt|bin>
    <name>-<YYYYMMDD>-<HHMMSS>-<ms>-log.json

The stamp is the log entry's own time - `logging.Stamp`, one
monotonic progression for the whole stream, so a request's log
line `time` names its snapshot family exactly, and two flows
never collide on a name. The extension follows the as-sent
cleaned content type: `json` anywhere in it picks `.json`,
`text/*` (event streams included) `.txt`, everything else `.bin`.
An empty body snapshots to an empty file.

Files are written as `.partial` and renamed on commit. A flow
that fails keeps its `.partial` - the partial payload is the forensic
artifact - while the log file still commits: the deferred
snapshot writer runs on every exit path.

## The tee points

Snapshots never force buffering: the capture happens at the
delivery points, the exact bytes that crossed.

- outgoing, buffered: `outBytes` written at the request build;
  committed once the outlet's `Do` succeeds.
- outgoing, streamed: `io.TeeReader` around `r.Body`; committed
  on the same `Do`.
- incoming, buffered: `respBytes` written before `w.Write`;
  committed after the write-back.
- incoming, streamed: `io.Copy` through an `io.MultiWriter`;
  committed when the copy completes.
- incoming, event lane: the `incomingStream` writer wrapped in
  the multiwriter, event by event; committed when `run()` ends
  clean.

Snapshot I/O failures never fail the flow: they land as error
objects on the request's log entry.

## The log file

`Dir.Log` writes `{"log": [...]}` - the entry's whole `Logs`
list, closing-flow logs included - once per distinct name, at
flow end: the deferred runs after `ClosingFlow`. Both kinds of
a name share the one file, marshaled in the stream's plain
form - so a logged `&` lands raw in the file too.

## The destination

`$REBEAR_HOME/snapshots`, else `$XDG_CONFIG_HOME/rebear/snapshots`
- resolved in `cli.Run` beside the config path, passed as
`ProxyOptions.Snapshots` (empty disables the feature, the
calls still validating and recording). The
directory is created lazily, at the first file.
