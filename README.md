# The 'rebear' tool

Programmable proxy for local HTTP introspection and manipulation.



## Contents

- [Overview](#overview)
- [Operation](#operation)
- [Examples](#examples)
  - [Protect a bearer token from a sandbox](#protect-a-bearer-token-from-a-sandbox)
  - [Select dynamically between two services](#select-dynamically-between-two-services)
  - [Set custom inference parameters](#set-custom-inference-parameters)
  - [Track costs over time](#track-costs-over-time)
  - [Log request and response details](#log-request-and-response-details)
  - [Snapshot the entire payload](#snapshot-the-entire-payload)
  - [Mutual TLS authentication](#mutual-tls-authentication)
- [Configuration reference](#configuration-reference)
- [Snippets](#snippets)
  - [opening-flow / closing-flow](#opening-flow--closing-flow)
  - [outgoing-headers / incoming-headers](#outgoing-headers--incoming-headers)
  - [outgoing-body / incoming-body](#outgoing-body--incoming-body)
  - [incoming-event](#incoming-event)
- [Parameters](#parameters)
  - [flow](#flow)
  - [outgoing](#outgoing)
  - [incoming](#incoming)
  - [secrets](#secrets)
  - [notes](#notes)
  - [headers](#headers)
  - [body](#body)
  - [event](#event)
- [Protecting secrets](#protecting-secrets)
- [TLS support](#tls-support)
  - [TLS on outlets](#tls-on-outlets)
  - [TLS on inlets](#tls-on-inlets)



## Overview

The _rebear_ command is a local reverse proxy oriented towards fine observation and control of HTTP
connections, via Starlark scripts that sit inline in its friendly
[cnf-based](https://github.com/canonical/cnf) configuration.

As a trivial example, this configuration will log the posted body on the way out to
[ntfy.sh](https://ntfy.sh), and the message ID of the response:

```python
inlets.local-ntfy:
    url: "http://localhost:8080/ntfy"
    outlet: "real-ntfy"
    midlets: ["add-title"]
    outgoing-headers:
        """
        log({"agent": headers["User-Agent"], "path": outgoing.path})
        """

midlets.add-title:
    outgoing-headers:
        """
        headers["Title"] = "Heads up!"
        """

outlets.real-ntfy:
    url: "https://ntfy.sh/"
    incoming-body:
        """
        get = body.json.get
        log({"id": get("id"), "title": get("title")})
        """
```

Then, try to run the following sample command resembling the one from the _ntfy.sh_ documentation:
```bash
$ curl -d "Backup successful" http://localhost:8080/ntfy/mytopic
```

This should generate logs similar to the following:
> {"time":"20261118-225055-977","method":"POST","path":"/ntfy/mytopic","out-path":"/mytopic","flow":["local-ntfy","add-title","real-ntfy"],"out-ms":1,"out-size":17,"code":200,"in-ms":458,"in-size":146,**"log":[{"agent":"curl/8.18.0","path":"/mytopic"},{"id":"uIHFQP2Z8NSs","title":"Heads up!"}]**}

A few observations:

- We define the local listening address in named _inlets_
- We define the connection to the remote service in _outlets_
- We optionally define one or more intermediate stages in _midlets_
- Each `log()` call became an element under the `"log"` log key
- The `"flow"` log key has shown the path out: _local-ntfy => add-title => real-ntfy_
- The `"/ntfy"` path prefix is _required_ by this inlet, and was _trimmed_
- Conversely, the outlet path is _prepended_ on the way out, but our outlet had no path
- We updated headers, and we could have updated the body too, both out and in
- The `outgoing-headers` snippet was used twice - snippets may be defined anywhere

This example shows the basics, but there's much more:

- Multiple inlets can listen on the same address with different paths
- Unix socket support, both for inlets and outlets
- Hot reload via SIGHUP without dropping connections
- Unbuffered support for SSE streams
- Selective named snapshots of incoming and outgoing content
- Atomic global state for recording and aggregating information persistently
- Mutation of most data in and out: headers, body, path, query, status code, etc

The following sections will detail those features.


## Operation

The configuration lives under the `$REBEAR_HOME` path, which defaults to
`~/.config/rebear/` if missing, respecting `$XDG_CONFIG_HOME`.

```
Usage: rebear [options]

Options:
  --indent                Indent log records as multi-line JSON.
  --log-at                Record snippet position in logs and errors.
  --add-tls <name>[,...]  Generate the named certificates.
  --add-secret <name>     Seal the named secret under secrets/.
  --salt <base64>         Set the seal's salt, for --add-secret.
  --keyring               Get master password from keyring via secret-tool.
  --set-keyring           Set master password in keyring via secret-tool.
```


## Examples

### Protect a bearer token from a sandbox

The sandbox client only knows its own throw-away key, while the real
service token lives in an encrypted secret file:

```bash
$ rebear --add-secret remote
password: ***
again: ***
secret: ***
secrets: wrote ~/.config/rebear/secrets/remote.json
```

In the configuration:

```python
inlets.sandbox:
    address: "127.0.0.1:8080"
    outlet: "remote"
    outgoing-headers:
        '''
        if headers.get("Authorization") != "Bearer the-sandbox-key":
                fail("invalid key", code=403)
        '''

outlets.remote:
    address: "some.remote.service:443"
    url: "/"
    outgoing-headers:
        '''
        headers["Authorization"] = "Bearer " + secrets["remote"]
        '''
```

### Select dynamically between two services

With `set_outlet` and `add_midlet` you can dynamically change the flow.

```python
inlets.local:
    address: "127.0.0.1:1234"
    outlet: "llama"

    outgoing-body:
        '''
        if body.json and body.json.get("model") == "remote":
            flow.set_outlet("remote")
        '''

outlets.llama:
    address: "127.0.0.1:11434"
    url: "http://127.0.0.1:11434"

outlets.remote:
    address: "some.remote.service:443"
    url: "/"
```

### Set custom inference parameters

```python
inlets.local:
    address: "127.0.0.1:1234"
    midlets: ["temperature"]

midlets.temperature:
    outlet: "remote"

    outgoing-body:
        '''
        if body.json:
            body.json["temperature"] = 1
        '''

outlets.remote:
    address: "some.remote.service:443"
```

### Track costs over time

With this setup the cumulative notes up to the current day are stored in `$REBEAR_HOME/notes/`.

```python
inlets.local:
    address: "127.0.0.1:1234"
    outlet: "remote"

outlets.remote:
    address: "some.remote.service:443"

    opening-flow:
        '''
        flow.state["cost"] = 0
        '''

    incoming-event:
        '''
        if event.json:
            flow.state["cost"] += event.json.get("cost", 0)
        '''

    closing-flow:
        '''
        notes["cost"] = notes.get("cost", 0) + flow.state["cost"]
        '''
```

### Log request and response details

The logs are embedded into the JSON line for the flow entry itself.

```python
inlets.sandbox:
    address: "127.0.0.1:8080"
    outlet: "remote"

    outgoing-headers:
        '''
        log({"user-agent": headers.get("User-Agent")})
        '''

    incoming-headers:
        '''
        log({"content-length": headers.get("Content-Length")})
        '''

outlets.remote:
    address: "some.remote.service:443"
```

With this in place, the JSON log for the respective request will include the key:
```json
"log":[{"user-agent":"rebear-test/1.0"},{"content-length":"113"}]
```

### Snapshot the entire payload

Snapshots may be requested on any snippet before the respective
body starts to stream out. The payload will be saved at

`$REBEAR_HOME/snapshots/<name>-<time>-{in,out,log}.{json,txt,bin}`

according to the used name, direction, and type of payload.

```python
inlets.sandbox:
    address: "127.0.0.1:8080"
    outlet: "remote"
    outgoing-headers:
        '''
        if headers.get("Suspect") == "1":
            incoming.snapshot("suspect")
            outgoing.snapshot("suspect")
        '''

outlets.remote:
    address: "some.remote.service:443"
```

### Mutual TLS authentication

This simple configuration implements authentication via client TLS
certificates (mutual TLS), with rebear talking to itself on both ends.
You can split these up and use other clients or remote services with
similar intent.

```python
inlets.auth-only:
    address: "127.0.0.1:8080"
    outlet: "remote"
    tls-certs: ["+rebear"]
    tls-auth: ["+client"]

outlets.remote:
    address: "some.remote.service"
```

The above will create both cert pairs (due to `+`). Copy both `tls/client.*`
files to the client for it to authenticate, and copy `tls/rebear.cert` to
the client for it to assert the remote authenticity. These paths are all
inside `$REBEAR_HOME`, both on the client and server rebear instances.

Then, on the other rebear on the client side that needs to connect to
the rebear above, use this setup:

```python
outlets.auth-only:
    address: "127.0.0.1:8080"
    tls-certs: ["rebear"]
    tls-auth: ["client"]
```


## Configuration reference

The following is an annotated reference of the complete configuration which
lives under `$REBEAR_HOME/rebear.cnf`.

```python
# The inlets define the entry points where flows start.
inlets:
    # Names start with a letter and may contain [a-z0-9-]
    <name>:
        # The address defines the network or unix socket listening
        # address for the inlet. If missing, the address defaults to the
        # url host and port. Unix sockets must start with "." or "/".
        # Multiple inlets may listen on the same address, in either format.
        address: "<host>:<port>|<unix socket>"

        # The url optionally constrains which requests flow through the inlet.
        # If a non-localhost name is provided, vhost-style matching is done.
        # If a path is defined, it is matched and then trimmed for the flow.
        # The path may optionally be provided alone. If missing, the host
        # defaults to the one in address.
        url: "[http[s]://<host>:<port>][<path>]"

        # The outlet and midlets fields define the default path for flows
        # coming through this inlet. They are both optional, but if both
        # are omitted a snippet of this inlet will necessarily need to
        # route the flow dynamically as otherwise there's no next step.
        outlet: "<name>"
        midlets: ["<name>"]

        # The behavior of the inlet may be tuned via flags that are
        # listed by name. Two flags are known at the moment:
        flags: ["<name>"]

        # The snippets define dynamic behavior for flows entering through this
        # inlet. Same as for midlets and outlets, every snippet is accepted,
        # both incoming and outgoing. Your organization defines the placement.
        <snippet-name>:
            '''
            <starlark>
            '''

# The midlets define the intermediate steps that flows may pass through.
midlets:
    # Names start with a letter and may contain [a-z0-9-]
    <name>:
        # If an outlet is defined, it will override the choice of the inlet
        # and any previous midlet.
        outlet: "<name>"

        # If midlets are defined, they are appended to the midlet list in the
        # provided order with duplications elided.
        midlets: ["<name>"]

        # The snippets define dynamic behavior for flows passing through this
        # midlet. Same as for inlets and outlets, every snippet is accepted,
        # both incoming and outgoing. Your organization defines the placement.
        <snippet-name>:
            '''
            <starlark>
            '''

# The outlets define the remote services that flows target.
outlets:
    # Names start with a letter and may contain [a-z0-9-]
    <name>:
        # The address defines the network or unix socket address to
        # connect to. If missing, the address defaults to the url host and
        # port. Unix sockets must start with "." or "/".
        address: "<host>:<port>|<unix socket>"

        # The url optionally defines the protocol, host, and path to use on
        # the outgoing request. If a path is provided, it is prepended to the
        # path on the outgoing flow. The path may optionally be provided alone.
        # External addresses default to https, while local ones default to
        # http unless the port is 443. The host defaults to the one in address.
        url: "[http[s]://<host>:<port>][<path>]"

        # The behavior of the outlet may be tuned via flags that are
        # listed by name. Two flags are known at the moment:
        flags: ["<name>"]

        # The snippets define dynamic behavior for flows leaving through this
        # outlet. Same as for inlets and midlets, every snippet is accepted,
        # both incoming and outgoing. Your organization defines the placement.
        <snippet-name>:
            '''
            <starlark>
            '''
```


## Snippets

Every snippet below may be used in an inlet, midlet, or outlet, according
to your organization. The snippets that share the same name are run in the
logical order: the inlet receives the _outgoing_ request towards the outlet,
so it runs the _outgoing_ snippet before the midlet, which runs before the
outlet, which then flushes it out. The outlet then receives the _incoming_
response towards the inlet, so it runs the _incoming_ snippet before the
midlet, which runs before the inlet, which then flushes it in.

### opening-flow / closing-flow

These snippets are run at the very start of the flow, before the request
information is available, and at the very end of the flow, after the
response has been fully flushed. These offer the right place for the initial
and final handling of flow data. In particular, the `closing-flow` snippet
is the _only_ snippet that has access to the atomic `notes` parameter.

- Available parameters: `flow`, `outgoing`, `incoming`, `notes` (closing only)
- Available on: inlets, midlets, outlets

### outgoing-headers / incoming-headers

These snippets are run when the headers have been processed and before
the body is handled. The outgoing direction is run when a request is going
towards the outlet, and the incoming direction is run when a response is
coming towards the inlet.

- Available parameters: `flow`, `outgoing`, `incoming`, `headers`
- Available on: inlets, midlets, outlets

### outgoing-body / incoming-body

These snippets are run after the body has been fully received:
the outgoing request body when inside `outgoing-body` or the
incoming response body when inside `incoming-body`. The body may
be changed within these snippets, and their presence anywhere
in a given flow will force the incoming or outgoing body to be
buffered.

- Available parameters: `flow`, `outgoing`, `incoming`, `body`
- Available on: inlets, midlets, outlets

### incoming-event

This snippet is run after each event of an SSE stream is received. Its
presence forces the buffering of the individual event only. Only the incoming
direction is available, run when an event is coming towards the inlet.

- Available parameters: `flow`, `outgoing`, `incoming`, `event`
- Available on: inlets, midlets, outlets


## Parameters

The snippets above have access to a selection of the following parameters.

### flow

The flow parameter is available on every snippet, and offers sequencing
control and data sharing.

- `flow.state` - A scratch dict to pass information between the snippets
  during the flow, and then discarded at the end.

- `flow.set_outlet(name)` - Sets the candidate outlet, but may still be
  replaced at any point before the outlet itself runs.

- `flow.add_midlet(name)` - Adds a midlet to the flow. If the midlet has
  already run or was already scheduled to run, the call does nothing.

### outgoing

The outgoing request as it currently stands. Every field may be assigned
before the outgoing request starts to be streamed through the outlet.

- `outgoing.method` - The request method (GET, POST, ...).

- `outgoing.path` - The request path as it currently stands. When an inlet
  has a path defined, that inlet is only used for requests that are prefixed
  with that path, and the path then loses the prefix and enters the flow.
  The path may then be altered by the flow, until it leaves through the
  outlet. Right before leaving, the path is prepended with the outlet path,
  if any, and then the path cannot be changed anymore.

- `outgoing.type` - The media type extracted from the Content-Type request
  header.

- `outgoing.headers` - The request header dict. Accepts changes until the
  request leaves through the outlet.

- `outgoing.query` - The request URL query parameters as a dict. The usual
  reading methods will return only the first occurrence of the given name,
  while the alternative `getn`, `popn`, and `popitemn` variants return the
  full list of values in order. Accepts changes until the request leaves
  through the outlet.

- `outgoing.snapshot(name)` - Schedule a full snapshot of the
  outgoing request payload under `$REBEAR_HOME/snapshots/<name>-*-out.*`.
  It may be called within any snippet until the request starts
  to be flushed through the outlet. The snapshot happens via streaming
  and does not force the buffering of the body.

### incoming

The incoming response as it currently stands. Every field may be assigned
before the incoming response starts to be streamed through the inlet.
Fields are set to `None` before the response arrives at the outlet.

- `incoming.code` - The response status code (200, 404, ...).

- `incoming.type` - The media type extracted from the Content-Type
  response header.

- `incoming.headers` - The response header dict. Accepts changes
  until the response leaves through the inlet.

- `incoming.snapshot(name)` - Schedule a full snapshot of the
  incoming response payload under `$REBEAR_HOME/snapshots/<name>-*-in.*`.
  It may be called within any snippet until the response starts
  to be flushed through the inlet. The snapshot happens via streaming
  and does not force the buffering of the body.

### secrets

A read-only dict holding the secrets that were loaded from the
encrypted `$REBEAR_HOME/secrets/` files. See the [Protecting secrets](#protecting-secrets)
section for details on how to create and manage them.

If there are no secrets at all, the `secrets` variable will not be available.

### notes

The `closing-flow` snippet is the only one with access to the `notes`
parameter: an atomic global annotation space, with the full dict API
available. The first time a `closing-flow` snippet touches the `notes`
value, the access is locked and no other snippet can touch the value
until the first snippet is finished.

The state persists across restarts under `$REBEAR_HOME/notes/<date>.json`.
The sequence of files offers a cumulative view, which means previous
days will offer a perspective on how the value has evolved rather
than being a snapshot of that day alone.

### headers

The outgoing request headers when inside `outgoing-headers` or the
incoming response headers when inside `incoming-headers`. The same
values are also available in other snippets as `outgoing.headers`
and `incoming.headers`, respectively.

The headers may be changed using the usual dict API until the
outgoing request headers are flushed through the outlet, or the
incoming response headers are flushed through the inlet.

### body

The outgoing request body when inside `outgoing-body` or the
incoming response body when inside `incoming-body`. The body may
be changed within these snippets, and their presence anywhere
in a given flow will force the incoming or outgoing body to be
buffered.

- `body.data` - The raw body bytes as a string.

- `body.json` - The decoded JSON value when appropriate, else None.
  Values with types other than JSON maps are also processed correctly.

Inside the respective snippets, either of these fields may be used
interchangeably for modifying the body.

### event

A server-side event inside an incoming SSE stream. The event may be
updated within the `incoming-event` snippet.

- `event.data` - The raw event data bytes as a string.

- `event.json` - The decoded event data JSON value when appropriate,
  else None. Values with types other than JSON maps are also processed
  correctly.

- `event.name` - The event name (the `event` stream field value),
  defaults to `"message"`.

- `event.id` - The event ID, an empty string when absent.

- `event.retry` - The reconnection hint in milliseconds, or
  `None` when missing.

All of these fields may be used for modifying the event, and the
`data` and `json` fields may both be used to change the event data,
interchangeably.


## Protecting secrets

### Storing

One important use case of rebear is verifying and replacing fake internal secrets
with real ones while the request is going out towards the remote service. Rather than
having the real secrets in plain text in the configuration file, rebear can encrypt
them at rest with a master password.

For that, start by creating a new secret:

```bash
$ rebear --add-secret remote
password:
again:
secret:
secrets: wrote ~/.config/rebear/secrets/remote.json
```

These secret files are self-contained: remove a file, and the secret is cleanly removed;
copy the same file, and the secret is backed up or reused elsewhere. Be aware that there's
a startup cost for every salt used, so if you plan on encrypting a secret locally for
sending elsewhere, use the `--salt` option to replicate the remote salt, if possible.

### Running

With one or more secrets in place, the next time rebear starts, it will ask for the
master password:

```bash
$ rebear
secrets password: 
```

Type the same password, and rebear will start as usual, except now every snippet has
access to the additional [secrets variable](#secrets):

```python
outlets.remote:
    outgoing-headers:
        '''
        headers["Authorization"] = "Bearer " + secrets["remote"]
        '''
```

Secrets also hot-reload on SIGHUP together with the rest of the configuration, but
for a change of master password, rebear needs to be restarted.

### Using the keyring

To avoid typing the master password at every restart, use `--keyring` to look it up on
the standard keyring. Before that works, initialize (or reset) the password
via `--set-keyring`.

Both flags leverage the `secret-tool` command-line tool to interact with the keyring,
so make sure that's installed and available in your `$PATH`.


## TLS support

Rebear has convenient TLS support both for inlets and outlets, including dynamic
generation of certificates and pinning of client certificates for authenticated
usage.

### TLS on outlets

#### Default behavior

TLS is on by default on outlets when using TCP connections. For example, this
setup will only establish a secure connection to the remote website, even if
nothing else was said about it:
```python
outlets:
    my-server:
        address: "192.168.0.100"
```

It will only establish the connection if the remote site is serving a proper
certificate for the given hostname or IP address that is recognized by the local
default certificates. It is possible to disable TLS explicitly, though, by using
the `url` parameter with the `http` scheme:
```python
outlets:
    my-server:
        address: "192.168.0.100"
        url: "http://192.168.0.100/"
```
In the specific case above, the `address` field could even be omitted entirely
as it defaults to the `url`'s host if the latter is present.

#### Custom certificates

Often for custom setups, though, your certificates will not be rooted at a
known CA, and yet when you are hitting a shared network you likely still
want your traffic to be private from every other device on the same network.

For this, you can _redefine_ the list of accepted certificates:
```python
outlets:
    my-server:
        url: "https://192.168.0.100/"
        tls-certs: ["+remote"]
```

This demonstrates a couple of aspects of the `tls-certs` option:
- The `remote` name means `$REBEAR_HOME/tls/remote.cert` is accepted.
- The `+` prefix means the certificate _pair_ is created if missing.

In addition to using the `+` prefix, another convenient way to generate
the certificates is via the CLI `--add-tls` option.

#### Default system certificates

When you use `tls-certs`, the certificates you provide are the _only_ ones accepted
by rebear. If for whatever reason you want to define certificates while still holding
the default ones, include the `"*"` certificate in the list:
```python
outlets:
    my-server:
        url: "https://web.server"
        tls-certs: ["remote", "*"]
```

#### Authorizing the outlet

The settings above allow fine control over when to accept the remote service
as the true service the outlet should be talking to, but it doesn't let the
remote service know who the client is. Besides using traditional payload-based
authentication methods for this, rebear supports client-side certificates:
```python
outlets:
    my-server:
        tls-certs: ["remote"]
        tls-auth: ["+local"]
```

The above will attempt to authenticate the client with the server via
`$REBEAR_HOME/tls/local.key`, which again will be generated if missing due
to the `+` prefix.

#### Forcing certificate trust

Typically certificates are only trusted if the remote service is reached via
the particular hostname or address that the CA has emitted the certificate for,
which is important when certificates are emitted by the same CAs for everybody.
If the destination identity isn't checked in those cases, anyone can request a
certificate for a different location and pretend to be the service you are
connecting to.

When dealing with bespoke local connectivity, though, this may be inconvenient
and potentially unnecessary, as long as the actual leaf key and certificate pair
are locked together for authentication. If that sounds sketchy, consider what's
the trust model when you establish an SSH connection to a server: you are not
trusting the name of the server, but rather the fact that the server holds a
private key for the public key you know it for, and the server is doing the same
with your client, ensuring your client holds the private side of the public key
it has for your account.

To support this scenario with rebear, you can set the flag `trust-certs`
explicitly:
```python
outlets:
    my-server:
        tls-certs: ["remote"]
        tls-auth: ["local"]
        flags: ["trust-certs"]
```

Rebear has a distinction between using certificates like this, which is locking
the client and server to a certificate pair, and just disabling verifications
completely, so if you try to use this flag while still having `"*"` in the certs
list, you'll get an error:
```
error: rebear.cnf: outlets.my-server: flag "trust-certs" needs tls-certs with pinned names and no default certs
```

If you _really_ want to disable TLS verification altogether, there's an additional
flag you may use for this:
```python
outlets:
    my-server:
        tls-certs: ["remote"]
        tls-auth: ["local"]
        flags: ["unsafe-tls"]
```

You are by yourself with that setup. Good luck. :)

#### TLS on unix socket outlets

Most of the settings above apply fine on unix sockets. Usually there's more trust in
these endpoints, though, because there's some implied protection from the filesystem
itself. That's why the default for them is `http` and not `https`, which makes the
simple cases simple. Still, as far as TLS goes, you can use it as usual with the
settings described above. You don't even need to spell `https` explicitly: as soon as
a TLS setting is used the outlet defaults to `https` again.

### TLS on inlets

The support for TLS on inlets allows connecting clients to encrypt their traffic and
also authenticate while speaking to rebear. Most of the settings look similar to what
you've seen above for outlets.

#### Default behavior

On inlets, the default behavior is to listen on http, as that's easy and comfortable
on a local machine setup, but that's not a good idea if your traffic is exposed on the
network, even if that's the local network. So if that's the case, read on.

#### Custom certificates

Securing the traffic for applications connecting to rebear is easy: just define the
desired custom certificate that the inlet will present to the application:

```python
inlets:
    my-server:
        address: "192.168.0.100"
        tls-certs: ["+rebear"]
```

This is analogous to the outlet behavior, but now on the listening side:
- The `rebear` name means `$REBEAR_HOME/tls/rebear.cert` is presented to connecting clients.
- The `+` prefix means the certificate _pair_ is created if missing.

As a reminder, in addition to using the `+` prefix, another convenient way to generate
the certificates is via the CLI `--add-tls` option.

#### Authorizing with the inlet

The settings above identify the inlet to the connecting clients, but it doesn't allow
rebear itself to restrict which clients can connect to the specific inlet. For that,
we again use the analogous option of outlets, this time requiring the client itself
to present the client-side certificate:
```python
inlets:
    my-server:
        tls-certs: ["rebear"]
        tls-auth: ["+client"]
```

The above will again identify the inlet to the client with the `rebear.cert`, and this
time it will also ensure that the client authenticates by presenting the key for
`$REBEAR_HOME/tls/client.cert` - as earlier, that certificate will be generated if
missing due to the `+` prefix.

When multiple inlets share the same address while using TLS, they must all use the same
TLS parameters as well. A client can only connect and flow via the inlet if it
authenticates properly and matches the inlet setup in the same way as when TLS is not
involved.
