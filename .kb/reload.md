# Preface

The SIGHUP reload: re-reading the configuration file and swapping
the serving to it, without closing the listening sockets and
without cutting the connections already flowing. `internal/cli`
drives it; `internal/proxy` owns the machinery.

IMPORTANT: Read the `.kb/agents.md` file.


# Concepts

## The semantics

SIGHUP re-reads the configuration file and serves the setup it
holds. Nothing else reloads - the flags and the log settings
stay from the start.

- The old serving stops consuming from the listeners at once:
  no new connection is accepted by the old generation.
- Connections already accepted keep being served to their end
  by the old generation - its program, its clients, its config.
  A keep-alive connection is closed after its in-flight
  response, so the next request arrives on the new generation.
  A streaming response rides the old generation until it ends.
- A listener the new setup still listens on is reused as-is: no
  rebind, no stale-socket removal, no refused-dial window.
- A listener the new setup dropped is closed and disposed: the
  unix socket path is unlinked.
- A reload that fails - bad file, bad snippet, a taken port -
  changes nothing: the current setup keeps serving and the
  failure goes to the log stream as a
  `SIGHUP - cannot reload configuration` line.
- Interrupt stays a hard stop that cuts every connection, the
  draining generations included - the process is leaving anyway.

## The master and the serving copy

`http.Server.Serve` only returns when its listener closes, and
a reload must stop the old accept loop while keeping the socket
alive. So one socket is held by two listeners:

- the master, from `net.Listen` - never served on. It lives in
  the proxy's listeners map and survives reloads.
- the serving copy, an independent descriptor over the same
  socket, from `File()` then `net.FileListener`. `Serve` runs
  on it, and closing it stops that generation's accept loop and
  nothing else.

The stdlib's unlink rules make the disposal fall out: a unix
master from `net.Listen` unlinks its path on close, a copy from
`net.FileListener` does not. Both kinds serve on a copy, first
generation included, so every generation retires the same way.

## The handover

The handover maps sockets by the inlet address, which parse
normalizes to name the socket one way everywhere - so the key
is stable across reloads however the config respells it. The
routing - vhost and mount - lives in the handlers, not the
socket, so any url change rides a reused listener, and so does
a tls change: the wrap rides the serving copy, the masters and
the handover untouched.

`Masters` returns the map; `ProxyOptions.Listeners` takes it
in. `New` takes custody: on success every inherited socket is
either serving in the new proxy or closed, and the caller's map
is never mutated - a failed reload leaves the handover intact
for the next try.

## The proxy API

- `New(opts)` builds without serving: compiles, groups, binds
  the fresh sockets, makes the serving copies. A failure closes
  only what the build created - the inherited sockets stay open,
  still owned by the old proxy.
- `Serve(ctx)` starts the accept loops and stops the proxy when
  the ctx ends. Infallible.
- `Retire` is the reload's stop: `server.Shutdown` under an
  already-cancelled context, which closes the serving copies
  and the idle connections and skips the wait. The active
  connections drain by themselves. The masters are untouched.
- `Stop` is the hard stop: servers, copies, and masters closed,
  unix paths unlinked. Safe on a retired proxy - its masters
  are shared with the generation that took them.

## The order

The new proxy is fully built before the old one retires, so a
failure leaves the running service untouched:

1. Load, parse, compile, build, bind the new keys, make the
   serving copies. Any failure aborts the reload.
2. Retire the old serving.
3. Serve the new.
4. Dispose the inherited sockets no new inlet listens on -
   after the binds, so a bind failure finds the old sockets
   still holding their ports and fails safe.

Two SIGHUPs racing coalesce in the signal channel; each pass is
a full reload. A SIGHUP during a drain needs nothing: the
draining generation is already retired.
