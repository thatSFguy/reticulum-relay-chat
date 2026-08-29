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
it is off by default.

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

> **This repository is currently private.** The download steps below
> need access to it. Everything else — building from a checkout, the
> config, the systemd unit — works regardless.

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
`reticulum-group-chat`. Requires **v0.5.0 or later**: earlier versions
discard link context `0xFB`, so no client can identify and the hub is
unusable.
