# RRC room links

**Status:** v2. Implemented by `rrc-hub` (this repo) and by
`reticulum-mobile-app`. **v2 adopts NomadNet's existing grammar; v1 was
a parallel invention and is superseded** — see §5.

RRC has no way to write down where a room is. "Come to #ops" is not
directions: there are many hubs, room names are not unique across them,
and nothing a person can say out loud carries which hub they meant. The
problem is sharpest in an offline mention notification, which arrives in
a general-purpose messaging app — the reader is told they were named in
`#ops` and has no way to get there.

This document describes the text form for "this room, on this hub".

---

## 1. It is not our format

NomadNet 1.2.8 (released 2026-07-24) ships a full RRC client and already
reads a link form. This document describes **that** form. It is not a
proposal and there is nothing here to adopt: the only thing an
implementation can do is match it or be the odd one out.

The reader is `Browser.handle_rrc_link` (`Browser.py:426-461`), reached
two ways:

- the **`rrc://` scheme** (`Browser.py:277-280`), checked before the
  target is parsed as a destination;
- an **`rrc@…` micron link**, whose shorthand `expand_shorthands` maps to
  `rrc.hub.session` (`Browser.py:206-214`, dispatched at `:312-314`).

Both hand the same payload to the same parser, so they are one grammar
with two spellings.

> `rrc.hub.session` looks like a destination aspect and is not one. It is
> the internal label `handle_link` switches on. The aspect an RRC hub
> registers is **`rrc.hub`** (`nomadnet/RRC.py:97` →
> `DEFAULT_DEST_NAME = "rrc.hub"`), which is what the optional
> `:<dest_name>` slot overrides.

---

## 2. Grammar

```
link           = scheme-form / shorthand-form / bare-form
scheme-form    = "rrc://" payload
shorthand-form = aspect "@" payload
bare-form      = payload
aspect         = "rrc" / "rrc.hub" / "rrc.hub.session"
payload        = [ "/" ] desthash [ ":" dest-name ] [ "/" room ]
desthash       = 32*32 HEXDIG     ; 16-byte truncated destination hash
dest-name      = 1*VCHAR          ; hub aspect; omitted = "rrc.hub"
room           = *VCHAR           ; literal, to the end of the payload
```

The canonical form a writer emits is the scheme form with a room:

```
rrc://43c8adb1172377a76b8f9ba41bb85e5c/ops
```

The scheme form is preferred over the shorthand for two reasons. It is
self-describing in a plain-text message body, which is where these links
mostly live; and upstream checks it *before* splitting the target on
`@`, so a room name containing `@` works in the scheme form and breaks
in the shorthand one (`link_target.split("@")` then yields three
components and falls through to the page fetcher).

Upstream's parse, transcribed (`Browser.py:430-446`):

```python
rest = link_target.strip()
if rest.startswith("/"): rest = rest[1:]
hub_part, _, room = rest.partition("/")
hex_part, _, dest = hub_part.partition(":")
dest = dest.strip() or None
hub_hash = bytes.fromhex(hex_part)      # must be 16 bytes
room = room.strip().lstrip("#").strip()
room_norm = room.lower() if room else None
```

### 2.1 The hash

Exactly 32 hexadecimal characters, lower-cased before use.

This is stricter than upstream, which only requires `bytes.fromhex` to
yield 16 bytes. `SPEC §11.6.3` is explicit about why the strict rule is
the right one: implementations should *"reject inputs with embedded
separators (`dead:beef:…`) — the wire form is plain bytes, accepting
forgiving variants creates aliases for the same destination and risks
cache-poisoning."* Being stricter than the grammar costs no interop: a
link a strict reader rejects is one no writer should have produced.

A writer that does not know its own destination hash **MUST** emit no
link rather than a partial one. A malformed link is pasted onward as
though it worked.

### 2.2 The room name

**The room segment is literal.** Everything after the first `/` in the
payload is the name, to the end of the token. Upstream does no
percent-decoding, so an encoded name joins a room whose name contains a
literal `%20`.

A reader **MUST** apply its ordinary room-name normalization — strip,
strip a leading `#`, lower-case — which is what upstream does inline and
what `rrc-hub`'s `normalizeRoomName` does for every other inbound name.
A link must not be able to address a room `JOIN` cannot reach.

Because the segment is literal and a link in running text ends at the
first whitespace, **a writer MUST NOT emit a link for a room name
containing whitespace.** It would be truncated on paste into a link
naming a shorter, different room. Such a room gets no link, and the
caller hides the share affordance. This is the one thing v1 could
express that v2 cannot, and it is the price of the segment being
literal — which is what makes every other name work in the client that
actually reads these.

Everything else round-trips: `:`, `@`, `/`, `%`, punctuation and
non-Latin scripts all survive, because the payload is split on its first
`/` and the rest is the name.

### 2.3 The `dest-name` slot

`rrc://<hash>:<dest_name>/<room>` names a hub running on a non-default
aspect. A writer omits it when the aspect is `rrc.hub`.

A reader that hardcodes the aspect — as `reticulum-mobile-app` currently
does — **MUST reject** a link naming another one rather than dial
`rrc.hub` anyway. Connecting to a different destination than the link
names is the exact class of failure this document exists to prevent.

### 2.4 There is no path namespace

v1 reserved `/room/` so a later target — a user, an invite, a hub
directory — could be added unambiguously. v2 has no such room: the whole
tail is the room name, so `rrc://<hash>:/page/index.mu` names a room
called `page/index.mu`.

This is a real loss, recorded here as a decision rather than an
oversight. A future target needs a different mechanism — a distinct
scheme, or a `dest-name` naming a different service.

---

## 3. What a client should do

**Reading.** Recognize the grammar in message bodies and in micron
pages, and offer the obvious action: connect to the hub at `desthash` if
not already connected, then `JOIN` the room. A payload with no room
names a hub only — show it, join nothing.

**Writing.** Anywhere a room is referred to out of context — a share
action, an invitation, a notification.

**Neither is required.** This is plain text in a message body. A client
that has never heard of it renders it as text, and a person can select
and copy it, which is strictly more than the room name alone gives them.

### 3.1 What it does not do

It does **not** make a link tappable in an arbitrary third-party client.
An offline mention notification is delivered over LXMF and read in the
recipient's own messaging app; that app linkifies what it chooses to,
and Android's stock `Linkify` covers http/https/rtsp/mailto only. In
Sideband it is a copy-paste token. It is a tap in NomadNet, in
`reticulum-mobile-app`, and in any client that implements §3.

---

## 4. Reference implementations

- **`internal/hub/rrclink.go`** in this repository — `ParseRRCLink` and
  `RRCLink.String()` as the round-trippable pair, with
  `internal/hub/rrclink_test.go` as the conformance cases: every form in
  §2, the strictness rules in §2.1, the v1 shim, and round trips over
  room names with separators and non-Latin scripts.
- **`reticulum-mobile-app`** — `shared/…/rrc/RrcRoomLink.kt` (grammar and
  encoding) and `shared/…/nomad/LinkTarget.kt` (dispatch).

`rrc-hub` emits links from:

- offline mention notifications (a link per room named) — the case the
  format exists for, since that message is read outside RRC entirely
- `/link [room]`, for handing a room to somebody who is not here

---

## 5. What happened to v1

v1 of this document (2026-08-29) defined:

```
rrc@<32hex>:/room/<percent-encoded name>
```

and argued that inventing an `rrc://` scheme would be an invention *"in
an ecosystem that already has a convention for exactly this"* — meaning
NomadNet's `nnn@` / `lxmf@` page-link shorthands.

The reasoning was right and the fact was wrong. NomadNet had shipped
`rrc://` **five weeks earlier**, in 1.2.8, and had already claimed the
`rrc@` shorthand for a different payload grammar. v1 was therefore the
parallel invention it set out to avoid.

The collision is not a clean failure. Run a v1 link through NomadNet's
parser and the hub resolves correctly while the room name comes out as
the literal string `room/<x>`:

| link | NomadNet resolves to |
|---|---|
| `rrc://<h>/ops` | hub `<h>`, room `ops` |
| `rrc@<h>/ops` | hub `<h>`, room `ops` |
| `rrc@<h>:/room/ops` *(v1)* | hub `<h>`, room **`room/ops`** |
| `rrc@<h>` | hub `<h>`, no room |

Nothing errors: a hub accepts any non-empty UTF-8 room name, so a
NomadNet user following a v1 link **creates and joins a junk room**. And
because v1 percent-encoded, `#off topic` shared as `off%20topic` would
join a room literally named `off%20topic`.

Readers **SHOULD** keep parsing v1 links — they exist, and shipped
clients shared them. The shim is unambiguous: a v1 link presents as an
*empty* `dest-name` (its trailing colon) plus a room of
`room/<segment>`, a shape §2's grammar cannot otherwise produce, since a
v2 link has either no colon at all or a non-empty `dest-name`. On that
one shape, and only that one, percent-decode the segment.

Writers **MUST NOT** emit v1 links.

The lesson is narrower than "check upstream first," which v1 did do. It
is that *upstream's convention for a neighbouring problem is not
upstream's convention for yours*: v1 read `SPEC §11.6.3`, generalised
the page-link grammar to a new aspect, and never checked whether the
client already had an RRC-specific answer. It did, in a file the same
project ships.
