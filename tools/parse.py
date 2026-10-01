#!/usr/bin/env python3
"""bench.log -> score.json"""
import json, re, sys
txt = open(sys.argv[1], errors="replace").read()
m = re.search(r"SCORE: (-?\d+) \(\+(\d+) (-?\d+)", txt)
score, plus, minus = (int(m.group(1)), int(m.group(2)), int(m.group(3))) if m else (0, 0, 0)
p = re.search(r"PASSED: (\w+)", txt)
e = re.search(r"Error (\d+) \(Critical:(\d+)\)", txt)
msgs = {}
for line in txt.splitlines():
    if re.search(r"ERROR\[|error:|失敗|Critical|脱落|整合性", line):
        k = re.sub(r"^\S+ ", "", line.strip())
        k = re.sub(r"ERROR\[\d+\]", "ERROR", k)
        k = re.sub(r"[0-9a-f]{8,}", "X", k)
        k = re.sub(r"\d+", "N", k)[:260]
        msgs[k] = msgs.get(k, 0) + 1
table = {}
tm = re.search(r"score\.ScoreTable\{(.*?)\n\}", txt, re.S)
if tm:
    for k, v in re.findall(r'"([^"]+)":\s+(\d+)', tm.group(1)):
        table[k.replace("/api/", "").replace(":competition_id", ":c")] = int(v)
wc = re.findall(r"WorkerCount: (map\[.*?\])\n", txt)
print(json.dumps({
    "pass": bool(p and p.group(1) == "true"), "score": score, "plus": plus, "minus": minus,
    "errors": f"{e.group(1)}(crit {e.group(2)})" if e else "?",
    "messages": [f"{n}x {k}" for k, n in sorted(msgs.items(), key=lambda x: -x[1])],
    "table": table, "workers": wc[-1] if wc else "",
}, ensure_ascii=False, indent=1))
