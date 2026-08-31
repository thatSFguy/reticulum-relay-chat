# rrc-hub

**An RRC hub that can reach you when you are not there.**

[Reticulum Relay Chat](https://rrc.kc1awv.net/) is IRC-style rooms over
the [Reticulum](https://reticulum.network/) mesh. A normal RRC hub relays
what is said between the people currently connected, and that is all —
close the app and the conversation happens without you, silently and
irrecoverably.

This one is a superset. It keeps a transcript so a room reads as a
conversation already in progress, and when somebody names you while you
are gone it **sends you a direct message in the LXMF client you already
use** — Sideband, MeshChat, the mobile app — with no registration step
and no change to any client.

It is a pure-Go, single-binary implementation with no runtime
dependencies, and it runs on a Raspberry Pi Zero.

---

## The one rule that shapes everything here

**Deployed clients cannot be modified.** Every feature below is built out
of things existing RRC clients already understand, or it does not ship.

That constraint is why this works at all. Replayed history is the
*original envelopes* re-sent — same message id, same timestamp — bracketed
by ordinary NOTICEs. Slash commands are ordinary MSG bodies. Offline
notifications ride LXMF, a protocol your client already speaks for other
reasons. A feature a client would have to be taught is a feature nobody
can use, so there are none.

---

## What it does that other hubs do not

### Offline mention notifications

Someone writes `@alice check this` in a room. Alice closed the app an
hour ago.

The hub notices the mention, sees Alice is not there, and sends the
notification to **her LXMF delivery destination** — the inbox her normal
messaging client is already listening on. It arrives as a real message
from a sender she can reply to.

Nobody registered anything. The address is a pure function of the public
key Alice proved when she last connected (SPEC §6.7.6 LINKIDENTIFY), and
an RNS destination hash is derived from that key and a name — so the hub
can compute where her client listens without a lookup, a directory, or a
protocol change.

Two routes are tried, and the difference between them matters:

1. **Direct** — straight to her delivery destination, blocking for *her
   own* §6.5 delivery proof. Success here is end-to-end evidence, and it
   is the common case: leaving a room is not leaving the mesh. An RRC
   client is a foreground app you close; your LXMF client is a background
   service on the same device that keeps announcing.
2. **Store-and-forward** — left with LXMF propagation nodes, held until
   her client next syncs.

Only the direct route clears a mention, because only it proves anything.
A propagation upload is acknowledged by the *node*, never the recipient —
so an uploaded mention **stays queued**, and is delivered over RRC on her
next visit regardless. The worst case is seeing it twice; the failure
this design refuses is seeing it never.

Resolution is deliberately conservative. RRC nicknames are advisory,
self-asserted and not unique, so `@alice` does not identify anyone on its
own. The hub resolves by hash prefix, then by nickname in the room, then
by nickname in its directory of people who have been here before — and
declines on ambiguity. A mention delivered to the wrong person is worse
than one delivered to nobody: it sends someone else's conversation to a
stranger.

### Room history and replay

With `history_enabled`, the hub keeps a bounded, expiring transcript per
room and replays the last few messages to anyone joining, so a room opens
as a conversation in progress rather than a blank screen. `/history` asks
for more.

Retaining plaintext conversation on disk is the operator's decision, so
it is off by default. Mentions are not: `mention_notify` and
`mention_lxmf` both default **on**, because off meant `@nick` silently
did nothing on every hub whose operator had not read the config closely
enough to find them.

Two consequences of that default, both deliberate. The hub retains a
directory of identities it has met (`peers.toml`) — that record is what
makes somebody addressable once their link is gone, and nothing can
notify an absent peer without it. And it announces its own
`lxmf.delivery` destination, so every messaging client on the mesh
lists it; a recipient who has never heard that announce holds no key to
verify a notification against and drops it in silence. Set either flag
to `false` to opt out.

### Commands that answer for the hub

`/help` lists the commands *you* can run and explains any one of them,
and the table it prints is the same table dispatch reads, so the two
cannot drift. It is the only place a newcomer is taught anything, which
is why its footer names the one diagnostic that matters. `/version` reports what is switched on here, which used to
be answerable only by `/stats` — an operator-only command.

The rest exist because mention notification fails **silently**, in five
distinct ways, and every one of them lands in a log the affected person
cannot read:

| | |
|---|---|
| `/notify` | your setting, how the hub would reach you, and what is stopping it |
| `/notify address` | the address a notification goes to — compare it against your messaging client's own, since a client using a *different identity* is the commonest cause of nothing arriving |
| `/notify test` | sends one through the production path now and reports the verbatim outcome (rate limited; it is real traffic) |
| `/mentions` | what the hub is holding for you, and when it last tried to send it |
| `/seen` | when someone was last heard from — and whether their nick is ambiguous, which is why a mention can resolve to nobody |
| `/link [room]` | a link to a room you can paste anywhere |
| `/whoami` | who the hub thinks you are |
| `/away`, `/back` | you are connected but not reading, so send rather than show |

`/away` closes a real gap: the hub's presence test can only measure
whether frames are arriving, so a client left open in a background tab
is indistinguishable from someone reading the room, and a mention in
that state is written into the room and nowhere else.

### Names that mean one person

RRC nicknames are advisory and not unique — `K_NICK` is a display hint.
That is fine until something has to act on a name, and then it is why
mentions fail: resolution declines rather than guess between two people
answering to "sam", so `@sam are you there` reaches neither of them and
says nothing about having failed.

So the hub **grants** a nick rather than accepting one. First claim
wins; later claimants become `sam1`, `sam2`. A name is owned by an
*identity*, so it survives disconnection — somebody who has been "sam"
here for a year does not come back to find a stranger holding it — and
the peer is told in an ordinary NOTICE, since nothing in RRC can carry
"you asked for sam and you are sam1".

Enforced at both entry points: `HELLO`, and the `K_NICK` a message may
carry, which the hub adopts as the session nick. Enforcing only the
first would be theatre. `unique_nicks = false` restores the old
behaviour.

### The notification address is not a person

A hub that sends mention notifications **must** announce an
`lxmf.delivery` destination — without it, recipients hold no public key
to verify the signature against and drop every notification in silence.
The cost is that messaging clients list that destination beside real
contacts.

So it announces as `<hub name>(noreply)` — derived, with no setting for
it. Whether that address reads replies is a fact about the software,
not a preference, and a free-form name could be made indistinguishable
from a person. The hub used to appear twice under one name, once as a
room to join and once as somebody to message; only the first was ever
true. Long names are truncated to fit; the `(noreply)` never is.

And it answers. Message that address and the hub replies once, saying
the address only sends, and giving the link to join the hub properly.
Before, that message went nowhere and the sender got silence — which
looks exactly like a broken hub.

### Rooms you can link to

"Come to #ops" is not directions: there are many hubs, room names are
not unique across them, and nothing said out loud carries which hub was
meant. So a room has a written form:

```
rrc@43c8adb1172377a76b8f9ba41bb85e5c:/room/ops
```

That is not an invention. It is the link-target syntax NomadNet already
uses (`SPEC §11.6.3`), with `rrc` as the aspect shorthand for `rrc.hub`,
exactly as `nnn` is shorthand for `nomadnetwork.node` and `lxmf` for
`lxmf.delivery`. Offline notifications carry one per room named — that
message is read outside RRC entirely, which is the case the format
exists for — and `/link [room]` prints one on demand for handing a room
to somebody who is not here.

It is plain text in a message body, so a client that has never heard of
it shows it as text somebody can copy — which is more than a room name
alone gives them. See [`docs/rrc-room-links.md`](docs/rrc-room-links.md)
for the grammar and what a client should do with it.

### Room names carry no `#`

The sigil is decoration a client adds when it renders a room. A client
that passes the typed line through used to create a room genuinely named
`#lobby`, thereafter displayed `##lobby` — a second room, distinct from
the one everyone else was in. Every room name is now normalized on the
way in, at JOIN, in command arguments, in `default_rooms` and on
registry load, so `#lobby` and `lobby` are one room.

### A lobby that is already there

A brand new hub used to be an empty prompt: no rooms, `/list` reporting
nothing, and the only way forward was already knowing that JOIN creates a
room by name. `default_rooms` (default `["lobby"]`) means the first person
to connect has somewhere to be.

### Everything a normal RRC hub does

Full parity with the reference Python hub
[`rrcd`](https://github.com/kc1awv/rrcd): `HELLO`/`WELCOME`,
`JOIN`/`PART`, `MSG`/`NOTICE`/`ACTION` fan-out, `PING`/`PONG`, `ERROR`,
RNS Resource transfer for large payloads; room modes (`+m` `+i` `+k` `+p`
`+t` `+n` `+r`, per-user `+o`/`+v`); `/list` `/who` `/topic` `/mode`
`/kick` `/ban` `/invite` `/register` and operator `/stats` `/reload`
`/kline`; server operators, klines, room founders, per-room bans and
invites; registered rooms and klines surviving restarts.

Every relayed message's `K_SRC` is rewritten to the link-verified
identity, so a client cannot spoof another's messages.

### Replies and reactions

RRC has no reply or reaction message, so a client that supports them
elsewhere cannot use them in a room. **[docs/rrc-extensions.md](docs/rrc-extensions.md)**
proposes a way to carry both that costs an unmodified client nothing: a
reserved envelope-key range the hub relays verbatim and never
interprets, so replies and reactions stay a client convention and the
hub stays transport-agnostic. Offered to other RRC implementations as a
convention rather than a change to RRC itself.

---

## Deploy it

### 1. Get a binary

Grab the binary for your machine, plus the example config for that same
version, from the [latest release](https://github.com/thatSFguy/reticulum-relay-chat/releases/latest):

```sh
gh release download --repo thatSFguy/reticulum-relay-chat \
  -p 'rrc-hub-linux-arm64' -p 'rrc-hub.example.toml' -p 'SHA256SUMS'
sha256sum -c --ignore-missing SHA256SUMS
```

Static, no dependencies, no runtime to install:

| Target | Asset |
|---|---|
| Linux x86-64 | `rrc-hub-linux-amd64` |
| Raspberry Pi 3/4/5 (64-bit OS) | `rrc-hub-linux-arm64` |
| Raspberry Pi 2/3/4 (32-bit OS) | `rrc-hub-linux-armv7` |
| Raspberry Pi 1 / Zero / Zero W | `rrc-hub-linux-armv6` |
| Windows x86-64 | `rrc-hub-windows-amd64.exe` |

```sh
chmod +x rrc-hub-linux-arm64
```

Or build it yourself — Go 1.26+, no cgo:

```sh
go build -o rrc-hub ./cmd/rrc-hub
```

### 2. Write a config

```sh
mv rrc-hub.example.toml rrc-hub.toml     # from the release, above
```

Take the config from the same release as the binary. Config keys move
between versions, and pairing a released binary with a config from a
branch tip is how you end up debugging a key your build does not have.

The example file documents every knob. A minimal hub with the
interesting features on:

```toml
[hub]
name = "my-hub"
greeting = "Welcome. /join #lobby to start, /help for commands."
identity_path = "hub_identity"

# A room to arrive in (default; set to [] for none)
default_rooms = ["lobby"]

# Rooms read as a conversation in progress
history_enabled = true

# Tell people they were named while they were away
mention_notify = true
mention_lxmf   = true

[[interfaces]]
type = "tcp_client"
address = "your.reticulum.node:7822"     # any RNS transport node
```

> **Pin a propagation node if you know one.** With
> `lxmf_propagation_node` unset the hub auto-selects, and it cannot know
> which node your users' clients actually sync from — a message left on
> the wrong one is never collected. The direct route does not care, and
> handles most deliveries anyway.

### 3. Run it

```sh
./rrc-hub -config rrc-hub.toml
```

On first run it generates a long-term Reticulum identity at
`identity_path` — **back that file up**, it is the hub's address. Startup
logs the destination hash:

```
RRC hub running — add this hub in a client by hash: 4252d9d9…
```

Paste that into an RRC client (the Rooms tab of `reticulum-mobile-app`,
for instance) and you are connected.

### 4. Keep it running

```ini
# /etc/systemd/system/rrc-hub.service
[Unit]
Description=RRC hub
After=network-online.target
Wants=network-online.target

[Service]
User=rrc
WorkingDirectory=/var/lib/rrc-hub
ExecStart=/usr/local/bin/rrc-hub -config /var/lib/rrc-hub/rrc-hub.toml
Restart=always
RestartSec=5
# State is a few small files next to the config
StateDirectory=rrc-hub

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl enable --now rrc-hub
journalctl -fu rrc-hub
```

The hub reconnects its own uplink, so a transport node restart, a NAT
timeout, or a dropped TCP connection recovers without help.

---

## Operating notes

**Keepalive must stay on.** `ping_interval` (30s) and `ping_timeout`
(60s) are what let the hub notice that somebody has gone. Set either to
`0` and it never learns, lists departed peers as present in rooms
forever, and mention notification stops working entirely — the people
most in need of an offline notification are precisely the ones the hub
would still believe were in the room.

**The announce cache is in memory only.** After a restart the hub knows
nobody, so the first delivery attempt to a given peer may fail while it
requests a path. It retries promptly rather than waiting out
`mention_push_interval`.

**Registered vs ephemeral rooms.** An unregistered room dies with its
last member, and a later room of the same name starts with a clean
transcript — that is RRC's contract. `/register` a room if you want its
history to survive everyone leaving. `default_rooms` are registered for
exactly this reason. Note `/unregister` does not purge a transcript; use
`/history purge`.

---

## Verified against real clients

A green test suite says the logic is self-consistent. It says nothing
about whether a real client can connect or a notification arrives, and
this project has been bitten by that repeatedly — several bugs where the
hub logged a *successful* outcome while nothing reached anyone.

So the paths that matter are exercised live on a public mesh, against
stock upstream **RNS 1.5.0 + LXMF 1.1.1** (the stack Sideband and the
mobile client are built on) as well as the Android client:

- link handshake, §6.7.6 LINKIDENTIFY, identity binding
- `HELLO`/`WELCOME`, `/list`, `JOIN`, history replay
- mention detection, queueing while absent, address derivation
  (cross-checked: the address the hub computes equals the one the
  recipient's own client computes for itself)
- **direct LXMF notification, delivered and signature-verified in a real
  client's inbox**
- propagation upload, node selection, and the retry behaviour around a
  cold announce cache

Not yet verified live: inbound RNS Resource reassembly from a client.

Store-and-forward retrieval is not something the hub can guarantee — some
propagation nodes accept an upload and then serve nothing back, to us and
to stock upstream LXMF alike. That is why an upload never clears a
mention.

---

## Layout

```
cmd/rrc-hub/        main() — flags, config load, signal handling
internal/
  rrc/              RRC wire protocol — CBOR envelope, constants, builders
  hub/              transport-agnostic hub: rooms, sessions, router, modes,
                    slash commands, fan-out, mentions, background loops
  roomreg/          rooms.toml + kline persistence
  history/          bounded, expiring per-room transcript store
  peerreg/          peers.toml — known identities, keys, pending mentions
  lxmfaddr/         derives a peer's LXMF address from its verified key
  service/          wires the hub to a live Reticulum stack; LXMF notifier
                    and propagation-node selection
  config/           TOML configuration loader
configs/            documented example configuration
```

The Reticulum stack — identity, packet, link, crypto, announce, TCP/HDLC
transport, Resource transfer, and the LXMF layer this hub's notifications
ride on — comes from
[`reticulum-go`](https://github.com/thatSFguy/reticulum-go), shared with
`reticulum-group-chat`. Requires **v0.7.0 or later**.

Two link-DATA contexts are load-bearing and both were once discarded by
the same `default: return`. Below v0.5.0, `0xFB` LINKIDENTIFY is dropped
and no client can identify at all — the hub is unusable. Below v0.7.0,
`0xFC` LINKCLOSE is dropped, so a clean disconnect goes unnoticed until
a 30-second poll finds it, and anything keyed on "is this person here?"
is wrong for that whole window — which for mention notification is the
difference between a message held and a message lost.
