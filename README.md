# reticulum-relay-chat

A pure-Go **Reticulum Relay Chat (RRC) hub** — the server side of an
IRC-style chat protocol layered on the [Reticulum](https://reticulum.network/)
network. Clients open a single Reticulum Link to the hub, identify, and
exchange CBOR-encoded envelopes; the hub relays room chat between them.

RRC protocol: <https://rrc.kc1awv.net/>. The reference hub is
[`kc1awv/rrcd`](https://github.com/kc1awv/rrcd) (Python); this is an
independent Go implementation.

## What it does

This hub targets feature parity with the reference Python hub `rrcd`.

- Announces an `rrc.hub` destination on the attached Reticulum network
  (`name_hash = SHA-256("rrc.hub")[:10] = ac9fd3a81e4036f86e1d`).
- Accepts inbound Reticulum Links, completing the LINKREQUEST → LRPROOF
  handshake, and binds each client's verified identity from its §6.6
  LINKIDENTIFY.
- Speaks the RRC envelope protocol: `HELLO`/`WELCOME`, `JOIN`/`JOINED`,
  `PART`/`PARTED`, `MSG`/`NOTICE`/`ACTION` fan-out, `PING`/`PONG`,
  `ERROR`, and `RESOURCE_ENVELOPE` for large payloads.
- Rewrites every relayed message's `K_SRC` to the link-verified identity
  hash, so a client can never spoof another's messages.
- Enforces hub-advertised limits and a per-session token-bucket rate
  limit.
- **Room modes** — `+m` moderated, `+i` invite-only, `+k` keyed, `+p`
  private, `+t` topic-locked, `+n` no-outside-messages, `+r` registered,
  plus per-user `+o`/`+v` (op/voice).
- **Slash commands** — `/list`, `/who`, `/topic`, `/mode`, `/kick`,
  `/op`/`/deop`/`/voice`/`/devoice`, `/ban`, `/invite`, `/register`/
  `/unregister`, and the operator commands `/stats`, `/reload`, `/kline`.
- **Operator / trust model** — `trusted_identities` server operators,
  server-wide klines, room founders, and per-room bans/invites.
- **Persistence** — registered rooms and klines survive restarts
  (`rooms.toml` and a kline file); a prune loop expires stale rooms.
- **Hub-initiated PING** keepalive with PONG-timeout link teardown.

## Layout

```
cmd/rrc-hub/        main() — flags, config load, signal handling
internal/
  rrc/              RRC wire protocol — CBOR envelope, constants,
                    message builders (ported from the verified Kotlin
                    implementation in reticulum-mobile-app)
  hub/              transport-agnostic hub: rooms, sessions, router,
                    modes, slash commands, fan-out, background loops —
                    driven through a Link interface
  roomreg/          rooms.toml + kline TOML persistence
  service/          wires the hub to a live Reticulum stack: identity,
                    TCP attach, rrc.hub destination, announce, link
                    routing, RNS Resource transfer, dead-link janitor
  config/           TOML configuration loader
configs/            example configuration
```

The Reticulum protocol stack — identity, packet, link, crypto,
announce, TCP/HDLC transport, Resource transfer — comes from
[`reticulum-go`](https://github.com/thatSFguy/reticulum-go)
(`github.com/thatSFguy/reticulum-go/rns`), the pure-Go RNS/LXMF module
shared with `reticulum-group-chat`. It was previously vendored here as
`internal/rns`; that copy has been removed in favor of the module, so
stack fixes and interop test coverage are shared rather than
forward-ported by hand. The module also carries an `lxmf` package
(messages, delivery, propagation-node store-and-forward), which this
hub does not use yet.

## Build & run

```sh
go build ./...
go test ./...
go build -o build/rrc-hub ./cmd/rrc-hub

cp configs/rrc-hub.example.toml rrc-hub.toml   # then edit
./build/rrc-hub -config rrc-hub.toml
```

On first run the hub generates a long-term Reticulum identity at the
configured `identity_path`. It logs its destination hash on startup:

```
RRC hub running — add this hub in a client by hash: <32 hex chars>
```

Paste that hash into an RRC client (e.g. the Rooms tab of
reticulum-mobile-app) to connect.

## Status

The RRC protocol layer, the hub room/session/command/mode logic, and
the TOML persistence layer are unit-tested (`internal/rrc`,
`internal/hub`, `internal/roomreg`). Behavior tracks the reference hub
`rrcd`; where `rrcd` and the published RRC spec diverge, `rrcd` is
followed (see `AGENTS.md`).

`internal/service` wires the hub to the responder side of the
`reticulum-go` link layer, which has not yet been exercised end-to-end
against a live client — that stack was written for an LXMF *initiator*,
so the responder-link path is the part most in need of live interop
verification. RNS Resource transfer is wired in both directions
(outbound send, inbound reassembly routed by `link_id`) but is likewise
unverified against a live client.
