# Preface

The secrets storage: seal files under the secrets directory,
the `--add-secret` and `--salt` options that write them, the
password ask at startup, and the predeclared `secrets` dict the
snippets read. `internal/secrets` owns the storage and the
crypto; `internal/snippets` binds the opened values; `internal/cli`
asks and opens.

IMPORTANT: Read the `.kb/agents.md` file.


# Concepts

## The storage

The secrets live one JSON file each under the secrets
directory, resolved the way snapshots, notes, and tls resolve
theirs: `$REBEAR_HOME/secrets`, else the user config dir's
`rebear/secrets`. The directory is the manifest - everything
in it is a secret, nothing outside it is one, and the cnf file
names none of it. The filename without `.json` is the secret's
name, held to the sniplet name rule.

    $REBEAR_HOME/secrets/remote.json
    {"salt":"…","nonce":"…","box":"…"}

The format is ours, and ours strictly: three fields, all
standard base64, in that order, the one JSON line closed by
a trailing newline. The salt and the nonce hold
exactly 24 bytes - 32 characters, no padding character in
sight - and the box holds the secret plus its 16-byte tag, so
at least 16. Unknown fields, missing fields, unpadded or
non-standard base64, and every wrong length fail the scan,
naming the file. A missing or empty directory is no secrets:
nothing asks, nothing binds.

## The seal and the ask

`scrypt` stretches the password against the salt into the
key - parameters n=32768, r=8, p=1, code constants in
`internal/secrets`, never file content, so a change of them
is a format change and old files stop opening. The box is
`nacl/secretbox`: XSalsa20-Poly1305, a fresh 24-byte nonce on
every seal.

`--add-secret <name>` is the one-shot that writes a file, like
`--add-tls`: the name is checked before stdin is read, the
password and the secret are read as two lines - hidden on a
terminal, plainly on a pipe
with no prompts - and the file lands 0600 in the 0700
directory, with a confirmation on stdout naming the file's
real path - the user's home shortened to `~` when the file
sits under it. On a terminal, every typed value is asked
twice, the second ask spelled `again: ` - and a differing
pair is no failure, for the wrong one cannot be named: the
pair is asked again until two reads agree. A pipe reads the
two lines alone. Overwriting an existing name is the rotation
path, not an error. The salt is picked in this order: the
`--salt <base64>` flag's own value, decoded to exactly 24 bytes
and with the directory unread; the directory's first secret's
salt in name order, so the common directory pays one
derivation; a fresh 24-byte draw on an empty directory. A
broken file fails the add there, naming the offender.

A salt borrowed from the directory pins the password: the add
verifies it opens the secrets the salt came from, refusing
otherwise as `wrong password: ...` - a differing one would
mint a directory no single password serves. The verification
is also the confirmation: the password is asked once only.
A wrong one is asked once more on a terminal; the second
failure ends the add.
The flag's salt joins no group, so it verifies nothing - the
second ask is what a terminal falls back to. The typed
secret keeps its own `again:` on a terminal, always: the
keyring skips the password's asks, never the secret's.

## The keyring

`--keyring` moves the password asks - the startup ask and
`--add-secret`'s - from stdin to the Linux keyring, through the
`secret-tool` CLI. The run asks its own keyring entry by two
attributes, the application name and the home - either
`$REBEAR_HOME` or the XDG tree, the same resolution the config
takes - so each home keeps its own password:

    secret-tool lookup application rebear REBEAR_HOME "$REBEAR_HOME"

The flag is a choice of the run, never saved. Under it stdin
is never touched for the password - a lookup that comes back
empty, or fails, is a hard error naming the home - and the
same verification rules hold, without the retry: a keyring
password that opens nothing of the borrowed-from secrets is
refused at once, and a fresh seal takes the entry whole, with
no second ask.

`--set-keyring` writes the entry, one-shot: the password is
asked for on stdin and checked first - the seals on disk
open with it, one ask, and with no seals a typed password is
asked again until the two agree - and a password that opens
nothing is refused as `wrong password: ...`. The confirmed
password is the stored one, and an empty one is refused
after the check: a later lookup cannot tell an empty entry
from a missing one.
The store carries the same attributes, so it updates the
entry in place - rotating is one `--set-keyring` again - and
the restart re-reads it. The two flags exclude each other and
the other one-shot options: one password flow per invocation.

## The startup

A directory holding anything asks for the password at startup -
`secrets password: ` on stderr, hidden on a terminal, a plain
line on a pipe, or the keyring's entry under `--keyring`.
The ask is a direct write: it never touches
the JSONL log stream, and neither does the password. Every
secret opens before serving starts; the first failure errors
naming its file, and nothing serves.

The password lives for the process. A SIGHUP rescans and
re-opens with it in hand - a file added, dropped, or resealed
rides the reload - and a sealed-open failure is a build
failure: `SIGHUP - cannot reload configuration` logs it and
the old generation keeps serving. A file resealed under a
different password fails the reload the same way. The first
ask tolerates one slip: a password that opens nothing is
asked once more, and the second failure ends the run.
Rebear never re-prompts after that; the operator restarts.

## The dict

The opened secrets reach the snippets as one predeclared
value, a frozen dict named `secrets`:

    headers["Authorization"] = "Bearer " + secrets["remote"]

The plaintext of a secret is a plain string. The dict is
frozen like the program's globals, so no snippet hands a
secret to another flow by mutation, and a setup with no
secrets binds nothing - a snippet naming it fails to compile
there. What a snippet writes into a request rides the wire
and may land in a snapshot or a log: the seal protects the
file on disk, not the traffic.

## Where the code sits

`internal/secrets` is the only package touching `scrypt` and
`nacl/secretbox`: `Scan` reads the manifest, `Salt` decodes
the flag, `Encrypt` seals, `Open` opens with the derived key
memoized by salt. `internal/cli` owns the asker - `x/term`
hidden reads, the signal guard around them, the plain pipe
path - and holds the password across generations. The proxy
stays crypto-blind: `ProxyOptions.Secrets` carries the opened
map to `snippets.Compile`, the same split the notes store
already follows.
