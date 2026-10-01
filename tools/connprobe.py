#!/usr/bin/env python3
"""TCP connect の所要時間を測り続ける（ベンチ中に別ノードから入口 :443 に向けて実行）。
usage: connprobe.py <ip> <port> <seconds>   → 5 秒ごとの件数 / p50 / p99 / max (ms)"""
import socket, sys, time
ip, port, secs = sys.argv[1], int(sys.argv[2]), float(sys.argv[3])
t0 = time.time(); res = []
while time.time() - t0 < secs:
    s = socket.socket(); s.settimeout(5); t = time.perf_counter()
    try:
        s.connect((ip, port)); d = (time.perf_counter() - t) * 1000
    except Exception:
        d = 5000.0
    s.close(); res.append((time.time() - t0, d)); time.sleep(0.01)
for b in range(0, int(secs), 5):
    v = sorted(d for t, d in res if b <= t < b + 5)
    if v: print("t=%3d-%3ds n=%4d p50=%7.2f p99=%7.2f max=%8.2f" % (b, b + 5, len(v), v[len(v)//2], v[int(len(v)*0.99)], v[-1]))
