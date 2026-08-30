# RRC extension keys — replies and reactions

**Status:** proposed, v1 draft. The hub-side contract in §1 is
implemented by `rrc-hub` (this repo) — see "How `rrc-hub` implements
it" at the end of §1, and `internal/hub/extensions_test.go` for the
proof. Client support is a separate question and is **not** claimed
here: check the client. Offered to other RRC implementations as a convention, not as a
change to RRC itself.

> An earlier revision of this document said "Implemented by `rrc-hub`
> and `reticulum-mobile-app`". That was the intent, not the state: the
> hub dropped every extension key on fan-out, because it rebuilds each
> envelope it forwards (to rewrite `K_SRC` to the link-verified
> identity) and `rrc.Envelope` had nowhere to keep a key it did not
> know. A client pair could have implemented replies perfectly and they
> would have vanished in transit, silently. Do not write "implemented"
> here before a test proves it.

RRC has no reply or reaction message. Clients that support both
elsewhere — the mobile app models them for LXMF today — cannot use them
in a room, because there is nowhere on the wire to say *which* message is
being replied to.

This document defines a way to carry that, chosen so that:

- **an unmodified client is unaffected.** No new message types, no
  changed semantics for anything that exists. A client that has never
  heard of this still renders every message it receives.
- **the hub never learns what a reaction is.** `internal/hub` is
  transport-agnostic by design; teaching it about emoji would be a
  one-way door. It carries some bytes and does not read them.
- **anyone can adopt it without asking permission.** The key numbers are
  in a range RRC does not use and states it will not use.

---

## 1. The extension key range

An RRC envelope is a CBOR map with unsigned-integer keys (`SPEC` /
`rrcd` `envelope.py`). Today keys `0..7` are defined:

| Key | Name | Owner |
|---|---|---|
| 0–7 | `K_V`, `K_T`, `K_ID`, `K_TS`, `K_SRC`, `K_ROOM`, `K_BODY`, `K_NICK` | RRC core |
| 8–63 | *unassigned* | **reserved for RRC core** |
| 64+ | *extensions* | this document, and anything that follows it |

A hub implementing this specification:

- **MUST** relay keys `≥ 64` verbatim on `MSG`, `NOTICE` and `ACTION`
  fan-out, and **MUST NOT** interpret them.
- **MUST** continue to drop keys `8..63`. They belong to a future RRC
  core, and a hub that passed them through would let a client
  pre-empt a key the spec has not assigned yet.
- **MUST NOT** allow an extension key to alter a core key. In
  particular `K_SRC` is rewritten to the link-verified identity before
  fan-out (that is what stops a client speaking as somebody else) and
  nothing in this range may change that.
- **MUST** bound the extension payload. See §5.

That is the whole hub-side contract. Everything below is a client
convention layered on top of it, and a second extension can be added
later without touching the hub again.

### How `rrc-hub` implements it

`rrc.Envelope.Ext` (`map[uint64]any`) holds keys `>= rrc.ExtKeyMin`.
`Decode` collects them, `Encode` re-emits them **after** the core keys
so nothing a client sends can overwrite one the hub set. Keys `8..63`
are not collected at all, so they continue to be dropped.

`Session.handleMsg` calls `Envelope.ExtEncodedLen()` and rejects the
frame when it exceeds `maxExtBytes` (128) — before fan-out, and with an
ERROR to the sender rather than a truncation.

`history.Record.Ext` (CBOR key 8, `omitempty`) retains them, so a
replayed reply still threads. Records written before this simply lack
the key and decode to nil.

Pinned by `internal/hub/extensions_test.go`: verbatim relay of every key
in §2, reserved keys still dropped, `K_SRC` unforgeable through the
extension range, oversized payloads rejected rather than truncated, and
threading surviving a replay.

---

## 2. Keys defined by this document

| Key | Name | Type | Meaning |
|---|---|---|---|
| 64 | `K_REPLY_TO` | 8 bytes | The `K_ID` of the message this one replies to. |
| 65 | `K_REACT_TO` | 8 bytes | The `K_ID` of the message this one reacts to. `K_BODY` is the reaction. |
| 66 | `K_REACT_OP` | uint | `0` = apply (default, may be omitted), `1` = retract. |

All three ride an **ordinary `MSG`** (type 20). No new message type is
defined, and none should be: an unknown type is dropped silently by
existing hubs and clients, so a reaction sent that way would be
invisible rather than degraded.

`K_REPLY_TO` and `K_REACT_TO` are mutually exclusive. A message carrying
both is malformed; a receiver **SHOULD** treat it as a reply and ignore
the reaction.

### Replies

```
K_T   = 20                       (MSG)
K_BODY = "yes, exactly that"     the reply text
K_REPLY_TO = h'a41b9c33d2e05f18' the K_ID being replied to
```

The body is the message. `K_REPLY_TO` only says what it is a response
to; a receiver that ignores the key still shows a perfectly sensible
line of chat.

### Reactions

```
K_T   = 20                       (MSG)
K_BODY = "👍"                     one emoji, the reaction
K_REACT_TO = h'a41b9c33d2e05f18' the K_ID being reacted to
K_REACT_OP = 1                   optional; 1 retracts
```

`K_BODY` **MUST** be a single user-perceived emoji (one grapheme
cluster), normalised to **NFC**. Senders **SHOULD NOT** emit variation
selectors beyond what NFC produces. Receivers **SHOULD** compare
reaction strings by exact byte equality after NFC, and **MAY** decline
to display anything that is not a single grapheme cluster — this keeps a
"reaction" from becoming an unbounded second message body.

**Apply and retract are idempotent, deliberately.** Reticulum is a lossy
mesh and a message may arrive twice; a toggle would flip twice and land
in the wrong state. Applying an already-present reaction is a no-op, and
so is retracting an absent one. This is why `K_REACT_OP` exists instead
of a toggle.

---

## 3. What a receiving client does

Reactions are **aggregated locally**, per message, keyed by the emoji,
holding the set of reactor identities. Nothing is aggregated on the wire
and nothing is aggregated by the hub. (`reticulum-mobile-app` already
stores exactly this shape in `StoredMessage.reactionsJson`.)

On an inbound `MSG` carrying `K_REACT_TO`:

1. Find the message whose `K_ID` matches, **within the same room**.
2. If it is not held, drop the reaction silently. Do not display it as a
   message. A client that joined recently legitimately does not have the
   target, and a stray emoji in the transcript is worse than nothing.
3. Otherwise add — or with `K_REACT_OP = 1` remove — the sender's
   identity from that emoji's set for that message.

The reactor identity is `K_SRC`, which the hub has rewritten to the
link-verified identity. **Reaction attribution is therefore as
trustworthy as message authorship**, which is a stronger guarantee than
most chat systems give it.

On an inbound `MSG` carrying `K_REPLY_TO`: display it as a reply to the
referenced message if held, and as an ordinary message if not. A reply
whose target has scrolled out of history is still a message worth
showing — unlike a bare reaction, which is meaningless without a target.

---

## 4. How this looks to a client that does not implement it

| Sent | Unmodified client shows |
|---|---|
| Reply | The reply text, as an ordinary message. The relationship is lost; nothing else is. |
| Reaction | A lone `👍` in the room, attributed to the reactor. |
| Retraction | A second lone `👍`. |

The reply case is lossless in every way that matters. The reaction case
is the honest cost of this design: an old client sees a little emoji
chatter it cannot fold away. That is a deliberate trade for the
alternative — a new message type, which existing hubs drop with
`unhandled inbound message type` and existing clients never see at all.

Clients that find the noise unacceptable in a mixed room may prefer to
offer reactions only when the hub advertises support. Hubs **MAY**
advertise it in the `WELCOME` capability map; that mechanism is left
undefined here until there is a second extension to justify it.

---

## 5. Security considerations

**A hub is a fan-out amplifier.** One inbound frame becomes one frame
per room member, so any bytes the hub relays without reading are
multiplied by the room size. A hub **MUST** cap the total encoded size
of extension keys — **128 bytes is recommended**, roughly ten times what
this document needs — and **MUST** reject, not truncate, a frame that
exceeds it. Without a cap, a reserved key range is an amplification
vector with the hub's return address on it.

**Extension keys are client-controlled and unauthenticated.** A client
may claim to reply to, or react to, any `K_ID`. The blast radius is one
room the sender is already in, and the effect is a misdirected reply
arrow. Attribution is unaffected: `K_SRC` is the hub's, not the
sender's.

**`K_ID` is chosen by the sender and unique only by chance.** It is 8
random bytes, and no hub enforces uniqueness. Accidental collision
within one room's history is negligible; deliberate reuse — a client
minting a `K_ID` that matches an earlier message — is cheap, and lets
reactions be steered onto the wrong line. A hub **MAY** reject a `MSG`
whose `K_ID` duplicates one seen recently in that room. Clients
**SHOULD** resolve a target only within the room the reference arrived
in, never across rooms.

**Reaction bodies are attacker-chosen text.** The single-grapheme rule
is what stops `K_BODY` becoming an unbounded second message channel that
bypasses whatever a client does to ordinary message rendering.

---

## 6. Hub implementation notes

Three places need to change in a hub shaped like this one:

1. **The envelope.** A decoder that parses into a fixed struct and
   re-encodes from it — as `internal/rrc/envelope.go` does — destroys
   unknown keys silently. It must retain keys `≥ 64` and re-emit them,
   still in canonical (ascending) CBOR key order.
2. **History.** A stored transcript record must persist the extension
   keys alongside the message, or replay after a rejoin hands clients
   messages whose reply anchors have been stripped, and every reply in
   the backlog flattens into a plain line.
3. **The size cap**, applied before fan-out and before storage.

Note that the hub already preserves `K_ID` through relay *and* through
history replay. That is the property everything here rests on, and it
was true before this document existed.

---

## 7. Test vectors

A canonical `MSG` reply, CBOR diagnostic notation:

```
{0: 1, 1: 20, 2: h'0102030405060708', 3: 1787923200000,
 4: h'6b621001912fd0bd5d6a33ae183fc56b', 5: "lobby",
 6: "yes, exactly that", 7: "Walden",
 64: h'a41b9c33d2e05f18'}
```

A reaction, and its retraction:

```
{0: 1, 1: 20, 2: h'1112131415161718', 3: 1787923201000,
 4: h'6b621001912fd0bd5d6a33ae183fc56b', 5: "lobby",
 6: "👍", 7: "Walden", 65: h'a41b9c33d2e05f18'}

{0: 1, 1: 20, 2: h'2122232425262728', 3: 1787923202000,
 4: h'6b621001912fd0bd5d6a33ae183fc56b', 5: "lobby",
 6: "👍", 7: "Walden", 65: h'a41b9c33d2e05f18', 66: 1}
```

**Watch the key ordering.** CBOR canonical order (RFC 7049) sorts map
keys by their *encoded* bytes — shorter encodings first, then
bytewise — not by numeric value. Keys `0..23` encode in one byte and
keys `24+` in two, so every extension key sorts after every core key
regardless of number. For the keys defined here that happens to be the
same as ascending numeric order, which is why a naive implementation
gets the right answer and a spec-correct one gets it for the right
reason. It stops coinciding the moment RRC core assigns a key above 23.

An implementation that encodes canonically is byte-identical to any
other, which is what makes these vectors testable.

---

## 8. Registry

New extensions take the next free key and are recorded here. Keys are
permanent once published; do not renumber.

| Key | Name | Defined by |
|---|---|---|
| 64 | `K_REPLY_TO` | this document, v1 |
| 65 | `K_REACT_TO` | this document, v1 |
| 66 | `K_REACT_OP` | this document, v1 |
| 67+ | *free* | |
