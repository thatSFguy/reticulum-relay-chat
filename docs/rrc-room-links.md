# RRC room links

**Status:** proposed, v1 draft. Implemented by `rrc-hub` (this repo).
Offered to other RRC implementations as a convention, not as a change to
RRC itself.

RRC has no way to write down where a room is. "Come to #ops" is not
directions: there are many hubs, room names are not unique across them,
and nothing a person can say out loud carries which hub they meant. The
problem is sharpest in an offline mention notification, which arrives in
a general-purpose messaging app — the reader is told they were named in
`#ops` and has no way to get there.

This document defines a text form for "this room, on this hub".

---

## 1. It is not a new format

NomadNet solved this problem years ago and the ecosystem already reads
its answer. `SPEC §11.6.3` documents the target syntax its browser
parses:

| Form | Meaning |
|---|---|
| `<32hex>` | a `nomadnetwork.node` at that destination hash |
| `<32hex>:/page/x.mu` | …with an explicit path |
| `nnn@<32hex>[:/path]` | `nnn` is shorthand for `nomadnetwork.node` |
| `lxmf@<32hex>` | open a conversation in the LXMF layer |

The shorthand expands through `Browser.py`'s `expand_shorthands`:
`nnn` → `nomadnetwork.node`, `lxmf` → `lxmf.delivery`.

**An RRC room link is the same grammar with one more shorthand:**

```
rrc@<32hex>:/room/<name>
```

`rrc` expands to `rrc.hub`, the aspect an RRC hub registers (`dest_name`
in `rrc-hub.toml`). Nothing else is new.

Choosing this over inventing a `rrc://` URI scheme is deliberate. A
scheme would be more familiar to someone arriving from the web, and it
would be tappable wherever something linkifies custom schemes — but it
would also be an invention, in an ecosystem that already has a
convention for exactly this, and adopting the existing one costs nothing
and composes with every tool that already parses it.

---

## 2. Grammar

```
link      = [ aspect "@" ] desthash [ ":" path ]
aspect    = "rrc" / "rrc.hub"
desthash  = 32*32 HEXDIG        ; 16-byte truncated destination hash
path      = "/room/" segment
segment   = *( unreserved / pct-encoded )
unreserved = ALPHA / DIGIT / "-" / "_" / "." / "~"
pct-encoded = "%" HEXDIG HEXDIG
```

The canonical form a writer emits is the shorthand with a room:
`rrc@<32hex>:/room/<name>`. A reader accepts every form above.

### 2.1 The hash

Exactly 32 hexadecimal characters, lower-cased before use.

`SPEC §11.6.3` is explicit about why this is strict: implementations
should *"reject inputs with embedded separators (`dead:beef:…`) — the
wire form is plain bytes, accepting forgiving variants creates aliases
for the same destination and risks cache-poisoning."* A link with a
short, spaced, or `0x`-prefixed hash is not a shorter link, it is a
different one.

A writer that does not know its own destination hash **MUST** emit no
link rather than a partial one. A malformed link is pasted onward as
though it worked.

### 2.2 The room name

RRC room names are arbitrary UTF-8 — spaces, punctuation and non-Latin
scripts are all legal — and a link is a whitespace-delimited token
somebody pastes out of a message body. So the segment is percent-encoded
over UTF-8 bytes, escaping **everything outside the unreserved set**.

That is stricter than a typical URL path encoder, which leaves `:` and
`@` alone. Both are structural here.

A reader **MUST** decode the segment and then apply its ordinary room
name normalization — for `rrc-hub` that means stripping a leading `#`,
since room names carry no sigil (it is display decoration a client adds)
and a link must not be able to address a room `JOIN` cannot reach.

### 2.3 Paths that are not `/room/`

A reader **MUST** reject a path it does not recognize rather than guess.
`/room/` is namespaced for the same reason NomadNet has `/page/` and
`/file/`: so a later target — a user, an invite, a hub directory — can
be added without ambiguity.

---

## 3. What a client should do

**Reading.** Recognize the grammar in message bodies and offer the
obvious action: connect to the hub at `desthash` if not already
connected, then `JOIN` the room. A link with no path names a hub only.

**Writing.** Anywhere a room is referred to out of context — a share
action, an invitation, a notification.

**Neither is required.** This is plain text in a message body. A client
that has never heard of it renders it as text, and a person can select
and copy it, which is strictly more than the room name alone gives them.
That property is the reason the format is text and not a new message
type.

### 3.1 What it does not do

It does **not** make a link tappable in a third-party client. An offline
mention notification is delivered over LXMF and read in the recipient's
own messaging app; that app linkifies what it chooses to, and a
NomadNet-style token is not a URL. In Sideband it is a copy-paste token.
It is a tap only in a client that has implemented §3.

A `rrc://` scheme would not change this materially — a custom scheme is
not linkified by Android's stock `Linkify` either, which covers
http/https/rtsp/mailto — so the tappability argument does not favour
inventing one.

---

## 4. Reference implementation

`internal/hub/rrclink.go` in this repository, with `ParseRRCLink` and
`RRCLink.String()` as the round-trippable pair, and
`internal/hub/rrclink_test.go` as the conformance cases — every form in
§2, the strictness rules in §2.1, and round trips over room names with
spaces, separators and non-Latin scripts.

`rrc-hub` emits links from:

- offline mention notifications (a link per room named) — the case the
  format exists for, since that message is read outside RRC entirely
- `/link [room]`, for handing a room to somebody who is not here
