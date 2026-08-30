#!/usr/bin/env python3
"""Message the hub's notification address and wait for its auto-reply."""
import os, sys, time, threading
import RNS, LXMF

HUB = bytes.fromhex(sys.argv[1])
got = threading.Event()

RNS.Reticulum(configdir=os.environ["RNS_CONFIG"])
idfile = os.environ.get("LXMF_ID", "/tmp/lxmfid")
identity = RNS.Identity.from_file(idfile) if os.path.exists(idfile) else RNS.Identity()
if not os.path.exists(idfile):
    identity.to_file(idfile)

router = LXMF.LXMRouter(identity=identity, storagepath=os.environ["RNS_CONFIG"] + "/lxmf")
dest = router.register_delivery_identity(identity, display_name="probe")
router.announce(dest.hash)
print(f"probe address: {RNS.hexrep(dest.hash, delimit=False)}")

def on_delivery(message):
    print("\n=== REPLY RECEIVED ===")
    print("title  :", message.title.decode("utf-8", "replace"))
    print("content:", message.content.decode("utf-8", "replace"))
    print("verified:", message.signature_validated)
    got.set()

router.register_delivery_callback(on_delivery)

# Let the announce propagate and a path to the hub resolve.
if not RNS.Transport.has_path(HUB):
    RNS.Transport.request_path(HUB)
for _ in range(150):
    if RNS.Transport.has_path(HUB):
        break
    time.sleep(0.2)
if not RNS.Transport.has_path(HUB):
    raise SystemExit("no path to the hub's lxmf.delivery destination")

hub_ident = RNS.Identity.recall(HUB)
hub_dest = RNS.Destination(hub_ident, RNS.Destination.OUT, RNS.Destination.SINGLE, "lxmf", "delivery")
msg = LXMF.LXMessage(hub_dest, dest, "hello?", "probe", desired_method=LXMF.LXMessage.DIRECT)
router.handle_outbound(msg)
print("sent; waiting for the auto-reply…")
got.wait(90)
