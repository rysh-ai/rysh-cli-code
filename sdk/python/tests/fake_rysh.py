"""A fake ``rysh`` binary for the SDK tests.

It records its argv (one JSON list per line) to $FAKE_RYSH_LOG and answers
from the rules in $FAKE_RYSH_SCENARIO (a JSON file):

    [{"has": ["wait", "--until"], "stdout": "...", "stderr": "", "exit": 0, "times": 1}, ...]

The first rule whose "has" elements all appear in argv answers. A rule with
"times" is used up after that many answers. No rule: exit 99. It never runs
anything it is given.
"""

import json
import os
import sys


def main() -> int:
    argv = sys.argv[1:]
    with open(os.environ["FAKE_RYSH_LOG"], "a", encoding="utf-8") as f:
        f.write(json.dumps(argv) + "\n")
    path = os.environ["FAKE_RYSH_SCENARIO"]
    with open(path, encoding="utf-8") as f:
        rules = json.load(f)
    for i, r in enumerate(rules):
        if all(h in argv for h in r.get("has", [])):
            if "times" in r:
                r["times"] -= 1
                if r["times"] <= 0:
                    rules.pop(i)
                with open(path, "w", encoding="utf-8") as f:
                    json.dump(rules, f)
            sys.stdout.write(r.get("stdout", ""))
            sys.stderr.write(r.get("stderr", ""))
            return int(r.get("exit", 0))
    sys.stderr.write("fake rysh: no rule for %r\n" % (argv,))
    return 99


if __name__ == "__main__":
    sys.exit(main())
