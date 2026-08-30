# Test clients

Two small Python clients for exercising a running hub against the
**upstream reference stack** — RNS and LXMF as Sideband and the mobile
app use them.

They exist because of `CLAUDE.md` §2: a green `go test ./...` says the
logic is self-consistent and nothing about whether a real client can
connect, be welcomed, or receive a notification. Every bug that has
cost this project a day was invisible to the unit suite and obvious the
first time a real client spoke to the hub — including several found
*with these scripts*: a command reply the client filed in a transient
banner, a `/notify test` answering into a link that had already closed,
and an auto-reply handed a destination hash where it wanted a public
key.

`adb` and the Android app are the other way to test, and not always
available. These need neither.

## Setup

```
python3 -m venv venv
venv/bin/pip install -r tools/requirements.txt

mkdir -p /tmp/rrctools
cp tools/rns-client.conf.example /tmp/rrctools/config
$EDITOR /tmp/rrctools/config          # point it at the hub's mesh
```

`share_instance = No` in that config is not optional: without it the
tools attach to whatever RNS instance is already running on the machine
and you are no longer testing what you think you are.

## `rrcclient.py` — an RRC client

Speaks the wire format in `internal/rrc/constants.go`: a CBOR map with
unsigned-integer keys over an established, identified RNS Link,
including §6.7.6 LINKIDENTIFY. Prints everything the hub sends back.

```
RNS_CONFIG=/tmp/rrctools RRC_ID=/tmp/rrc-id \
  venv/bin/python tools/rrcclient.py <hub-dest-hash> <nick> [command...]
```

Each command is one of:

| form | effect |
|---|---|
| `join:<room>` | JOIN |
| `say:<room>:<text>` | MSG |
| `saynick:<room>:<nick>:<text>` | MSG asserting a different `K_NICK` |
| `wait:<seconds>` | pause |
| anything else | sent as a MSG to `$RRC_ROOM` (default `lobby`) |

So a slash command is just passed through — `/help`, `/notify test`.

Environment: `RNS_CONFIG` (required), `RRC_ID` (identity file, created
on first use — reuse it to keep the same identity across runs, which is
what nick ownership and mention queueing are keyed on), `RRC_ROOM`,
`RRC_LINGER` (seconds to stay connected after the last command; raise
it for anything that answers slowly).

Two identity files means two peers, which is how to test nick
uniqueness and mentions between them.

**It does not implement RNS Resources.** A hub sending a long command
reply as a Resource will see it cancelled and fall back to chunked
NOTICEs — correct behaviour on both sides, but do not read it as the
hub failing.

## `lxmfping.py` — an LXMF probe

Registers an `lxmf.delivery` identity, announces it, messages a
destination, and prints any reply with its `signature_validated` state.

```
RNS_CONFIG=/tmp/rrctools LXMF_ID=/tmp/lxmf-id \
  venv/bin/python tools/lxmfping.py <lxmf-dest-hash>
```

Used for the hub's notification address — the reply proves the hub
answers somebody who messages it, and that the signature verifies
against an announce the recipient actually heard.

A hub logs both its destination hashes at startup:

```
RRC hub "…" — dest_name=rrc.hub dest_hash=<rrc>  identity=…
announced lxmf.delivery (<lxmf>)
```
