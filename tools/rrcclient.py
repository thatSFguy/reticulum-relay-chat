#!/usr/bin/env python3
"""A minimal RRC client, for driving a hub from the command line.

Speaks the wire format in internal/rrc/constants.go: a CBOR map with
unsigned-integer keys over an established, identified RNS Link.

  rrcclient.py <hub-dest-hash-hex> <nick> <command> [command...]

Each command is either a slash command or "join:<room>" / "say:<room>:<text>".
Everything the hub sends back is printed.
"""
import os, sys, time, threading
import RNS, cbor2

KV, KT, KID, KTS, KSRC, KROOM, KBODY, KNICK = range(8)
THELLO, TWELCOME = 1, 2
TJOIN, TJOINED, TPART, TPARTED = 10, 11, 12, 13
TMSG, TNOTICE, TACTION = 20, 21, 22
TPING, TPONG, TERROR = 30, 31, 40
NAMES = {TWELCOME: "WELCOME", TJOINED: "JOINED", TPARTED: "PARTED",
         TMSG: "MSG", TNOTICE: "NOTICE", TACTION: "ACTION",
         TPING: "PING", TPONG: "PONG", TERROR: "ERROR"}

class Client:
    def __init__(self, dest_hex, nick, idfile):
        self.nick = nick
        self.link = None
        self.ready = threading.Event()
        self.lock = threading.Lock()

        RNS.Reticulum(configdir=os.environ["RNS_CONFIG"])
        if os.path.exists(idfile):
            self.identity = RNS.Identity.from_file(idfile)
        else:
            self.identity = RNS.Identity()
            self.identity.to_file(idfile)

        dest_hash = bytes.fromhex(dest_hex)
        if not RNS.Transport.has_path(dest_hash):
            RNS.Transport.request_path(dest_hash)
            for _ in range(100):
                if RNS.Transport.has_path(dest_hash):
                    break
                time.sleep(0.1)
        ident = RNS.Identity.recall(dest_hash)
        if ident is None:
            raise SystemExit(f"no announce for {dest_hex}; is the hub running?")
        dest = RNS.Destination(ident, RNS.Destination.OUT, RNS.Destination.SINGLE, "rrc", "hub")
        self.link = RNS.Link(dest)
        self.link.set_link_established_callback(self.on_up)
        self.link.set_packet_callback(self.on_packet)

    def on_up(self, link):
        # SPEC §6.7.6 LINKIDENTIFY — the hub binds the peer identity from
        # this, and reaps any session that never sends it.
        link.identify(self.identity)
        self.send(THELLO, body={0: "rrcclient.py", 1: "0.1"}, nick=self.nick)

    def send(self, typ, room=None, body=None, nick=None):
        env = {KV: 1, KT: typ, KID: os.urandom(8), KTS: int(time.time() * 1000),
               KSRC: self.identity.hash}
        if room is not None: env[KROOM] = room
        if body is not None: env[KBODY] = body
        if nick is not None: env[KNICK] = nick
        RNS.Packet(self.link, cbor2.dumps(env)).send()

    def on_packet(self, data, packet):
        try:
            env = cbor2.loads(data)
        except Exception as e:
            print(f"  [undecodable frame: {e}]"); return
        t = env.get(KT)
        if t == TPING:
            self.send(TPONG, body=env.get(KBODY)); return
        if t == TWELCOME:
            self.ready.set()
        room = env.get(KROOM)
        body = env.get(KBODY)
        who = env.get(KNICK)
        tag = NAMES.get(t, f"type{t}")
        prefix = f"[{tag}]"
        if room: prefix += f" #{room}"
        if who: prefix += f" <{who}>"
        if isinstance(body, (dict, list)):
            print(f"{prefix} {body}")
        elif body is None:
            print(prefix)
        else:
            for line in str(body).split("\n"):
                print(f"{prefix} {line}")

def main():
    dest_hex, nick = sys.argv[1], sys.argv[2]
    cmds = sys.argv[3:]
    c = Client(dest_hex, nick, os.environ.get("RRC_ID", "/tmp/rrcid"))
    if not c.ready.wait(20):
        raise SystemExit("no WELCOME within 20s")
    time.sleep(0.5)
    for cmd in cmds:
        if cmd.startswith("join:"):
            c.send(TJOIN, room=cmd[5:])
        elif cmd.startswith("say:"):
            _, room, text = cmd.split(":", 2)
            c.send(TMSG, room=room, body=text, nick=nick)
        elif cmd.startswith("saynick:"):
            _, room, newnick, text = cmd.split(":", 3)
            c.send(TMSG, room=room, body=text, nick=newnick)
        elif cmd.startswith("wait:"):
            time.sleep(float(cmd[5:])); continue
        else:
            c.send(TMSG, room=os.environ.get("RRC_ROOM", "lobby"), body=cmd, nick=nick)
        time.sleep(1.2)
    time.sleep(float(os.environ.get("RRC_LINGER", "2")))
    c.link.teardown()
    time.sleep(0.3)

main()
