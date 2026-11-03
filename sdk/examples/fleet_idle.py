#!/usr/bin/env python3
"""Run a fleet of 3 claude agents and wait for all of them to go idle.

    python3 sdk/examples/fleet_idle.py [session-name]

Each agent opens in its own pane beside yours; you can watch and take over
any of them. If one stops for a permission prompt, this script reports it and
leaves it for you: the SDK never answers or approves.
"""

import sys
from concurrent.futures import ThreadPoolExecutor

sys.path.insert(0, __file__.rsplit("/", 2)[0] + "/python/src")  # run from a checkout

import rysh

TASKS = [
    "Read README.md and list three unclear sentences. Do not edit files.",
    "List the TODO comments under cmd/. Do not edit files.",
    "Summarise what docs/scripting.md says about exit codes. Do not edit files.",
]


def watch(pane: rysh.Pane) -> str:
    # needs-you also ends the wait: a pane waiting on a human is not idle,
    # and waiting for idle alone would sit there until the timeout.
    while True:
        r = pane.wait_for("any", timeout=0)
        if r.state in ("idle", "needs-you", "exited"):
            return r.state
        try:
            r = pane.wait_for("idle", timeout=60)
            return r.state
        except rysh.RyshTimeoutError:
            continue  # still working (or now needs you): look again


def main() -> int:
    s = rysh.Session(sys.argv[1] if len(sys.argv) > 1 else None)
    panes = [s.spawn(agent="claude", prompt=t) for t in TASKS]
    for p in panes:
        print(f"started {p.title} ({p.id})")
    with ThreadPoolExecutor(len(panes)) as pool:
        states = list(pool.map(watch, panes))
    for p, st in zip(panes, states):
        print(f"{p.title}: {st}")
    return 0 if all(st == "idle" for st in states) else 1


if __name__ == "__main__":
    sys.exit(main())
