"""Throwaway accounts and a room on a local Conduit (127.0.0.1), then exec of the live harness.

Called by scripts/live-matrix-local.sh only:
    python3 -I live-matrix-local-setup.py <port> <go binary> <ready secs>

Every password and access token is generated or received in this process and handed to go test
only through its environment (execve). Nothing credential-shaped is printed, logged or put in argv.
stderr: ids and HTTP statuses. stdout: nothing until go test's own -json output.
Exit 3 = infrastructure (server not ready, an account step failed); else go test's rc.
"""
import json
import os
import secrets
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

PORT, GO, READY_SECS = sys.argv[1], sys.argv[2], int(sys.argv[3])
ROOT = f"http://127.0.0.1:{PORT}"
BASE = ROOT + "/_matrix/client/v3"
KEEP = ("PATH", "HOME", "TMPDIR", "USER", "LOGNAME", "LANG", "LC_ALL", "TERM", "SHELL")


def step(msg):
    print(time.strftime("%H:%M:%S"), msg, file=sys.stderr, flush=True)


def infra(msg):
    step("INFRA: " + msg)
    sys.exit(3)


def req(method, path, body=None, token=None, expect=(200,)):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method)
    r.add_header("Content-Type", "application/json")
    if token:
        r.add_header("Authorization", "Bearer " + token)
    name = path.split("/")[1].split("?")[0]
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            return resp.status, json.loads(resp.read() or b"{}")
    except urllib.error.HTTPError as e:
        try:
            d = json.loads(e.read() or b"{}")
        except Exception:
            d = {}
        if e.code in expect:
            return e.code, d
        # The status and the server's errcode only, never a body that could echo input.
        infra(f"{method} {name} -> HTTP {e.code} {d.get('errcode', '')}")
    except Exception as e:
        infra(f"{method} {name} -> {type(e).__name__}")


def register(username):
    """Register with the dummy stage; returns (user_id, access_token). The password is never kept."""
    pw = secrets.token_urlsafe(24)
    status, d = req("POST", "/register", {"username": username, "password": pw}, expect=(200, 401))
    if status == 401:
        flows = [f.get("stages") for f in d.get("flows", [])]
        if ["m.login.dummy"] not in flows:
            infra(f"register {username}: no m.login.dummy flow offered")
        status, d = req("POST", "/register", {"username": username, "password": pw,
                                              "auth": {"type": "m.login.dummy", "session": d.get("session")}})
    if not d.get("access_token") or not d.get("user_id"):
        infra(f"register {username}: no access token or user id returned")
    return d["user_id"], d["access_token"]


step("waiting for /_matrix/client/versions")
deadline = time.time() + READY_SECS
while True:
    try:
        with urllib.request.urlopen(ROOT + "/_matrix/client/versions", timeout=5) as resp:
            if resp.status == 200:
                break
    except Exception:
        pass
    if time.time() > deadline:
        infra(f"server not ready within {READY_SECS} s")
    time.sleep(1)
step("server ready")

bot_id, bot_tok = register("rysh-bot")
echo_id, echo_tok = register("rysh-echo")
obs_id, obs_tok = register("rysh-observer")
if len({bot_tok, echo_tok, obs_tok}) != 3:
    infra("two accounts got the same access token")

# A three-member room (not a 1:1), unencrypted: the mention rule decides what is addressed, which is
# the path a real team room takes. The echo user creates it and invites the other two.
_, room = req("POST", "/createRoom", {"preset": "private_chat", "name": "rysh live", "invite": [bot_id, obs_id]}, echo_tok)
room_id = room.get("room_id", "")
if not room_id.startswith("!"):
    infra("createRoom returned no room id")
for tok in (bot_tok, obs_tok):
    req("POST", "/join/" + urllib.parse.quote(room_id, safe=""), {}, tok)
step(f"accounts {bot_id} (bot), {echo_id} (echo), {obs_id} (observer); room {room_id} (3 members, unencrypted); tokens not shown")

env = {k: os.environ[k] for k in KEEP if k in os.environ}
env.update({
    "GOFLAGS": "-mod=readonly",
    "GOTOOLCHAIN": "local",
    "RYSH_LIVE_MATRIX_SERVER_URL": ROOT,
    "RYSH_LIVE_MATRIX_BOT_TOKEN": bot_tok,
    "RYSH_LIVE_MATRIX_ROOM_ID": room_id,
    "RYSH_LIVE_MATRIX_ECHO_TOKEN": echo_tok,
})
args = [GO, "test", "-tags", "livechannels", "-run", "^TestLiveMatrix", "-json", "-count=1",
        "-timeout", "10m", "./internal/channels/"]
step("exec: go " + " ".join(args[1:]))
sys.stderr.flush()
os.execve(GO, args, env)
