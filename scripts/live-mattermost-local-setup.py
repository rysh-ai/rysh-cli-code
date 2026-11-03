"""Throwaway accounts on a local Mattermost (127.0.0.1), then exec of the live harness.

Called by scripts/live-mattermost-local.sh only:
    python3 -I live-mattermost-local-setup.py <port> <go binary> <ready secs>

Every password and token is generated or received in this process and handed to go test only
through its environment (execve). Nothing credential-shaped is printed, logged or put in argv.
stderr: ids and HTTP statuses. stdout: nothing until go test's own -json output.
Exit 3 = infrastructure (server not ready, an account step failed); else go test's rc.
"""
import json
import os
import secrets
import string
import sys
import time
import urllib.error
import urllib.request

PORT, GO, READY_SECS = sys.argv[1], sys.argv[2], int(sys.argv[3])
BASE = f"http://127.0.0.1:{PORT}/api/v4"
KEEP = ("PATH", "HOME", "TMPDIR", "USER", "LOGNAME", "LANG", "LC_ALL", "TERM", "SHELL")


def step(msg):
    print(time.strftime("%H:%M:%S"), msg, file=sys.stderr, flush=True)


def infra(msg):
    step("INFRA: " + msg)
    sys.exit(3)


def password():
    # Meets Mattermost's default policy; never printed.
    alphabet = string.ascii_letters + string.digits
    return "".join(secrets.choice(alphabet) for _ in range(20)) + "Aa1!"


def req(method, path, body=None, token=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method)
    r.add_header("Content-Type", "application/json")
    if token:
        r.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            raw, hdr = resp.read(), resp.headers
    except urllib.error.HTTPError as e:
        # The status and the server's error id only, never a body that could echo input.
        try:
            eid = json.loads(e.read() or b"{}").get("id", "")
        except Exception:
            eid = ""
        infra(f"{method} {path.split('/')[1]} -> HTTP {e.code} {eid}")
    except Exception as e:
        infra(f"{method} {path.split('/')[1]} -> {type(e).__name__}")
    return (json.loads(raw) if raw else {}), hdr


step("waiting for /system/ping")
deadline = time.time() + READY_SECS
while True:
    try:
        with urllib.request.urlopen(BASE + "/system/ping", timeout=5) as resp:
            if json.loads(resp.read()).get("status") == "OK":
                break
    except Exception:
        pass
    if time.time() > deadline:
        infra(f"server not ready within {READY_SECS} s")
    time.sleep(2)
step("server ready")

# A fresh server: the first user is the system admin.
admin_pw = password()
req("POST", "/users", {"email": "admin@rysh-live.invalid", "username": "rysh-admin", "password": admin_pw})
_, hdr = req("POST", "/users/login", {"login_id": "rysh-admin", "password": admin_pw})
admin = hdr.get("Token")
if not admin:
    infra("login returned no session token")
req("PUT", "/config/patch", {"ServiceSettings": {"EnableBotAccountCreation": True, "EnableUserAccessTokens": True}}, admin)

team, _ = req("POST", "/teams", {"name": "rysh-live", "display_name": "rysh live", "type": "O"}, admin)
echo, _ = req("POST", "/users", {"email": "echo@rysh-live.invalid", "username": "rysh-echo", "password": password()}, admin)
req("PUT", f"/users/{echo['id']}/roles", {"roles": "system_user system_user_access_token"}, admin)
bot, _ = req("POST", "/bots", {"username": "rysh-bot", "display_name": "rysh bot"}, admin)
bot_uid = bot["user_id"]
for uid in (echo["id"], bot_uid):
    req("POST", f"/teams/{team['id']}/members", {"team_id": team["id"], "user_id": uid}, admin)
chan, _ = req("POST", "/channels", {"team_id": team["id"], "name": "live", "display_name": "live", "type": "O"}, admin)
for uid in (echo["id"], bot_uid):
    req("POST", f"/channels/{chan['id']}/members", {"user_id": uid}, admin)
step(f"team {team['id']}, echo user {echo['id']}, bot user {bot_uid}, channel {chan['id']}")

bot_tok, _ = req("POST", f"/users/{bot_uid}/tokens", {"description": "rysh nightly"}, admin)
echo_tok, _ = req("POST", f"/users/{echo['id']}/tokens", {"description": "rysh nightly"}, admin)
if not bot_tok.get("token") or not echo_tok.get("token"):
    infra("a token step returned no token")
step("tokens issued for the bot and the echo user (values not shown)")

env = {k: os.environ[k] for k in KEEP if k in os.environ}
env.update({
    "GOFLAGS": "-mod=readonly",
    "GOTOOLCHAIN": "local",
    "RYSH_LIVE_MATTERMOST_SERVER_URL": f"http://127.0.0.1:{PORT}",
    "RYSH_LIVE_MATTERMOST_BOT_TOKEN": bot_tok["token"],
    "RYSH_LIVE_MATTERMOST_CHANNEL_ID": chan["id"],
    "RYSH_LIVE_MATTERMOST_ECHO_TOKEN": echo_tok["token"],
})
args = [GO, "test", "-tags", "livechannels", "-run", "^TestLiveMattermost", "-json", "-count=1",
        "-timeout", "10m", "./internal/channels/"]
step("exec: go " + " ".join(args[1:]))
sys.stderr.flush()
os.execve(GO, args, env)
