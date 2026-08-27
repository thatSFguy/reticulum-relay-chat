# RRC client parity checklist

Everything the Go RRC hub now exposes that a **client** (e.g. the
reticulum-mobile-app) must support to be at parity. Grouped by what the
client must **send** and what it must **handle**. The hub tracks the
reference Python hub `rrcd`; where `rrcd` and the published RRC spec
diverge, `rrcd` is followed.

## 1. Message types

| Type | Code | Direction | Client must |
|---|---|---|---|
| `HELLO` | 1 | client → hub | unchanged |
| `WELCOME` | 2 | hub → client | unchanged (but see §4 — no caps map) |
| `JOIN` | 10 | client → hub | unchanged; carries `+k` key in the body |
| `JOINED` | 11 | hub → client | unchanged; member-list body is optional |
| `PART` | 12 | client → hub | unchanged |
| `PARTED` | 13 | hub → client | unchanged |
| `MSG` | 20 | client ⇄ hub | unchanged |
| `NOTICE` | 21 | hub → client (mostly) | unchanged |
| `ACTION` | **22** | client ⇄ hub | **NEW** — send for `/me`-style messages; render inbound type-22 like `MSG`. Routed/fanned-out identically to `MSG`. |
| `PING` | 30 | client ⇄ hub | hub may now send these unprompted (§4) |
| `PONG` | 31 | client ⇄ hub | must answer hub `PING` (§4) |
| `ERROR` | 40 | hub → client | body is a plain string (§5) |
| `RESOURCE_ENVELOPE` | **50** | client ⇄ hub | **NEW** — large-payload transfer (§6) |

## 2. Slash commands

A `MSG` **or** `NOTICE` whose body is a string beginning with `/` is
treated by the hub as a hub-local **command**: it is **consumed — not
echoed or forwarded**. The hub answers with a `NOTICE` (informational)
or `ERROR` (denied / bad usage). An unknown command yields
`ERROR "unrecognized command"`.

Client requirements:
- Be able to send any `/...` text as a `MSG`/`NOTICE`.
- Display `NOTICE`/`ERROR` replies that arrive after a command.
- Ideally provide affordances for the common commands.

Commands (any user unless noted):
- `/list` — registered, non-private rooms
- `/who [room]` (alias `/names`) — member list of a room
- `/topic <room> [text]` — view (no text) or set the topic
- `/mode <room> <±flag> [arg]` — set room / user modes
- `/kick <room> <nick|hashprefix>` — room-op
- `/op` `/deop` `/voice` `/devoice` `<room> <target>` — room-op
- `/ban <room> add|del|list [target]` — list: any; add/del: room-op
- `/invite <room> add|del|list [target]` — room-op
- `/register <room>` / `/unregister <room>` — room founder
- `/stats`, `/reload`, `/kline add|del|list [target]` — server operators only

> `ACTION` (type 22) is **not** command-dispatched — only `MSG`/`NOTICE`
> bodies are scanned for a leading `/`.

## 3. Room modes

Room modes: `+m` moderated, `+i` invite-only, `+k` keyed, `+p` private,
`+t` topic-locked, `+n` no-outside-messages, `+r` registered. Per-user:
`+o` op, `+v` voice.

Client requirements:
- Parse the **mode string** the hub broadcasts:
  `NOTICE "mode for <room> is now: <modestring>"`. A mode string is `+`
  followed by the set flags in the fixed order `i k m n p r t`
  (e.g. `+int`), or `(none)`.
- Parse the **topic broadcast**:
  `NOTICE "topic for <room> is now: <topic or (cleared)>"`.
- To join a `+k` room, send the room key as the **JOIN body**
  (`K_BODY`, a string).
- Expect and surface mode-driven rejections — see §5.

## 4. Session behaviors (new / changed)

- **WELCOME carries no capabilities map** — body key 2 is omitted. Do
  not depend on it.
- **JOIN room-info NOTICE** — immediately after `JOINED`, the joiner
  receives:
  `NOTICE "room <r>: <registered|unregistered>; mode=<modestring>; topic=<topic or (none)>"`.
  Parse and display it.
- **No unsolicited room directory** — the hub no longer pushes an
  "Active rooms: …" NOTICE on connect. Use `/list` for discovery.
- **Re-HELLO resets the session** — sending `HELLO` again drops the
  client from all rooms and re-welcomes it; usable as an explicit reset.
- **Hub-initiated PING** — the hub may send `PING` unprompted; the
  client **must** reply `PONG` echoing the body, or its link is torn
  down. (Active only when the operator configures `ping_interval`.)
- **Rate limiting** is a token bucket — the client may receive
  `ERROR "rate limited"`; back off and retry.
- **Server bans** — a banned identity is disconnected at link-identify
  time: `ERROR "banned"` followed by link teardown.
- **Nick (`K_NICK`, key 7)** — send the nick in `HELLO` and on every
  `MSG`/`NOTICE`; the hub re-stamps it on forwarded messages. The
  legacy HELLO-body key `64` is still accepted on input.
- **Greeting (MOTD)** — sent as one or more `NOTICE`s after `WELCOME`;
  a large greeting may instead arrive as a Resource of kind `motd`.

## 5. ERROR strings to handle gracefully

```
send HELLO first            rate limited                banned
too many rooms              JOIN requires room name     PART requires room name
message requires room name  invite-only (+i)            bad key (+k)
banned from room            banned from <room>          room is moderated (+m)
no outside messages (+n)    not authorized              not authorized (+t)
kicked from <room>          unrecognized command        resource transfer disabled
resource too large: <n> > <max>
```

## 6. Resource transfer (large payloads)

- Capability key `CapResourceEnvelope = 0` in the HELLO / WELCOME caps
  map (advisory — presence is the signal).
- `RESOURCE_ENVELOPE` (type 50) body is a CBOR map:
  `0` id (8 bytes), `1` kind (string), `2` size (uint, bytes),
  `3` sha256 (32 bytes, optional), `4` encoding (string, optional).
- Kinds: `notice`, `motd`, `blob`.
- Flow: the sender emits a `RESOURCE_ENVELOPE`, then transfers the
  payload bytes as an **RNS Resource** on the link; the receiver matches
  the Resource to the envelope by size (and sha256 when present). The
  hub forwards a fully-received `notice`-kind payload to the room as a
  `NOTICE`.

## 7. Room history — nothing to implement

The hub can retain a bounded, expiring transcript per room
(`history_enabled`) and replay recent messages to a joining client.
**A client needs no changes for this**, which is the point: the deployed
clients cannot be updated, so the feature is built entirely out of what
they already handle.

What a client sees when it joins a room with history:

1. `JOINED`, then the room-info `NOTICE`, exactly as before.
2. A `NOTICE` reading `--- N message(s) from earlier ---`.
3. N ordinary `MSG` / `ACTION` envelopes.
4. A `NOTICE` reading `--- end of history ---`.

The replayed envelopes are the originals: same `K_ID`, same `K_TS`, same
`K_SRC`, same `K_NICK`. That has two consequences worth knowing:

- A client that remembers message ids can recognise a replayed message
  it already rendered and skip it. One that does not will simply render
  it again, which is correct behavior for a client seeing it for the
  first time.
- `K_TS` is the time the message was **sent**, not the time it was
  replayed, so a client that sorts or displays by timestamp shows the
  conversation in its original order without doing anything special.

A client that wants to do better than the default can match the two
bracket NOTICEs to collapse the block, grey it out, or suppress
notification sounds for it — but none of that is required, and the
brackets are plain text either way.

`/history [room] [count]` asks for more than the join replay carried,
and `/history purge <room>` (room operators) drops a transcript. Both
are ordinary slash commands — a `MSG` whose body starts with `/` — so
any client that can send text already supports them.

## 8. Mention notifications — nothing to implement

With `mention_notify`, a message that names someone reaches them even if
they were not in the room. As with history, **a client needs no
changes**: everything arrives as ordinary `NOTICE` text.

A client is named by either form:

- `@nick` — resolved against the room's members first, then against
  identities the hub has seen before. An ambiguous nickname resolves to
  nobody, because RRC nicknames are advisory and not unique.
- `@<hashprefix>` — `@` plus 6 or more hex characters of an identity
  hash. Exact, and the way to be certain.

What the named client receives depends on where it is:

| Where they are | What arrives |
|---|---|
| In the room | Nothing extra — fan-out already delivered the message |
| Connected, elsewhere | `NOTICE` in that room: `you were mentioned in <room> by <who>: <text>` |
| Not connected | Held; delivered after `WELCOME` on their next session |

Held mentions arrive as a `--- N mention(s) while you were away ---`
`NOTICE` followed by one `NOTICE` per mention, each carrying the room,
how long ago, who, and a truncated quote.

`/notify on|off` (and bare `/notify` to check) lets any client opt out.
Like every other slash command it is just a `MSG` body starting with
`/`, so nothing needs to be added to a client to support it.

> **Note for client authors:** a client that wants a distinct mention
> UX can match the `you were mentioned in ` and
> `--- N mention(s) while you were away ---` prefixes, but this is
> optional — the text reads correctly as-is.

## 9. Wire constants (reference — unchanged)

- Envelope keys: `KV`=0 (version, must be 1), `KT`=1 (type), `KID`=2
  (8 random bytes), `KTS`=3 (ms since epoch), `KSrc`=4 (16-byte identity
  hash), `KRoom`=5, `KBody`=6, `KNick`=7. Envelope is a CBOR map with
  unsigned-integer keys.
- HELLO body keys: `0` name, `1` version, `2` capabilities,
  `64` legacy nick.
- WELCOME body keys: `0` hub name, `1` hub version, `2` capabilities
  (not sent by this hub), `3` limits map.
- WELCOME limits map keys `0`–`4`: max nick bytes, max room-name bytes,
  max msg-body bytes, max rooms per session, rate limit (msgs/min).
