#!/bin/bash
# ベンチ中に叩く: 対象ノードの CPU プロファイルを 30 秒取り、top を表示
set -u; . "$(dirname "$0")/env.sh"
n=${1:-1}; h=$(host_of $n); out="$ROOT/measurements/pprof-isu$n-$(date +%H%M%S).pprof"
$SSH $h "curl -fsS -o /tmp/cpu.pprof 'http://127.0.0.1:6060/debug/pprof/profile?seconds=${SECS:-30}'" && scp -q -o LogLevel=ERROR $h:/tmp/cpu.pprof "$out" && go tool pprof -top -nodecount=45 "$out" 2>/dev/null | tee "${out%.pprof}.txt" | head -60
