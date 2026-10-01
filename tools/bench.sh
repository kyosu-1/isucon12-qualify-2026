#!/bin/bash
# 1 回叩けば記録まで終わる: ログ初期化 → リソース記録 → ベンチ → 収集 → scores/log.md
set -u; . "$(dirname "$0")/env.sh"
NOTE="${NOTE:-}"; TS=$(date +%Y%m%d-%H%M%S); D="$ROOT/measurements/$TS"; mkdir -p "$D"
SECS=${SECS:-95}
for n in $NODES; do
  $SSH $(host_of $n) "
    sudo truncate -s0 /var/log/nginx/access.log /var/log/nginx/error.log 2>/dev/null
    sudo truncate -s0 /var/log/mysql/slow.log 2>/dev/null
    sudo pkill pidstat; sudo pkill vmstat
    nohup sh -c 'LC_ALL=C pidstat -u 1 $SECS > /tmp/pidstat.txt' >/dev/null 2>&1 &
    nohup sh -c 'vmstat -t 1 $SECS > /tmp/vmstat.txt' >/dev/null 2>&1 &
  " &
done
$SSH $BENCH "sudo pkill vmstat; nohup sh -c 'vmstat -t 1 $SECS > /tmp/vmstat.txt' >/dev/null 2>&1 &" &
# ベンチ機 → isu1:443 の同時接続数（ベンチ機の connect() は片方の偶奇のポート約 3.2 万本を使い切ると遅くなる）
$SSH $ISU1 "nohup sh -c 'for i in \$(seq 1 30); do ss -tan state established \"( sport = :443 )\" | wc -l; sleep 3; done > /tmp/conns.txt' >/dev/null 2>&1 &" &
wait
# 疎通確認（プールの死んだ接続を捨てる）
$SSH $ISU1 'curl -sk -o /dev/null -w "warmup /api/me -> %{http_code}\n" --resolve admin.t.isucon.local:443:127.0.0.1 https://admin.t.isucon.local/api/me'
$SSH $BENCH "sudo -u isucon bash -c 'ulimit -n \$(ulimit -Hn); cd /home/isucon/bench && ./bench -target-url https://t.isucon.local -target-addr $ISU1_IP:443 ${BENCH_OPTS:-}'" > "$D/bench.log" 2>&1
python3 "$ROOT/tools/parse.py" "$D/bench.log" > "$D/score.json"
{
  echo "commit: $(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null) ($(git -C "$ROOT" log -1 --format=%s 2>/dev/null))"
  echo "dirty: $(git -C "$ROOT" status --porcelain -- webapp etc tools | wc -l | tr -d ' ')"
  echo "nodes:$NODES"; echo "note: $NOTE"
} > "$D/meta.txt"
ALP_M='^/api/organizer/player/[^/]+/disqualified,^/api/organizer/competition/[^/]+/finish,^/api/organizer/competition/[^/]+/score,^/api/player/player/[^/]+,^/api/player/competition/[^/]+/ranking,^/js/,^/css/,^/img/'
$SSH $ISU1 "sudo alp ltsv --file /var/log/nginx/access.log --sort sum -r -m '$ALP_M' -q --qs-ignore-values -o count,method,uri,min,avg,max,sum,p99,2xx,3xx,4xx,5xx 2>&1 | head -40" > "$D/alp.txt"
for n in $NODES; do
  h=$(host_of $n)
  $SSH $h "awk '\$1==\"Average:\"{next} \$NF==\"Command\"{secs++;next} NF>=10 && \$(NF-2)~/^[0-9.]+\$/{cpu[\$NF]+=\$(NF-2)} END{for(c in cpu) printf \"%-24s %6.1f\n\", c, cpu[c]/secs}' /tmp/pidstat.txt | sort -k2 -rn | head -8" > "$D/cpu-isu$n.txt"
  $SSH $h "cat /tmp/vmstat.txt" > "$D/vmstat-isu$n.txt"
  $SSH $h "curl -s 'localhost:3000/internal/stats?routes=1'" | python3 -c "
import json,sys
try:
    for r in json.load(sys.stdin): print('%8d  avg %8.3fms  max %9.1fms  %s' % (r['n'], r['avg_ms'], r['max_ms'], r['path']))
except Exception as e: print('no stats', e)" > "$D/routes-isu$n.txt"
  $SSH $h "sudo journalctl -u isuports --since '-3min' --no-pager -o cat | grep -iE 'error|panic' | sed -E 's/[0-9a-f]{8,}/X/g; s/[0-9]+/N/g' | cut -c1-240 | sort | uniq -c | sort -rn | head -20" > "$D/app-errors-isu$n.txt"
  if grep -qx mysql "$ROOT/etc/isu$n/services" 2>/dev/null; then
    $SSH $h "sudo test -s /var/log/mysql/slow.log && sudo pt-query-digest --limit 12 /var/log/mysql/slow.log 2>/dev/null | head -150" > "$D/slow.txt"
  fi
done
$SSH $BENCH "cat /tmp/vmstat.txt" > "$D/vmstat-bench.txt"
busy() { awk '$15 ~ /^[0-9]+$/ && $15 < 90 {n++; b += 100-$15} END {if(n) printf "%.0f%%(%ds)", b/n, n; else printf "idle"}' "$1"; }
CONNS=$($SSH $ISU1 "sort -n /tmp/conns.txt | tail -1")
BENCHCPU=$(busy "$D/vmstat-bench.txt")
SCORE=$(python3 -c "import json;d=json.load(open('$D/score.json'));print(d['score'])")
PASS=$(python3 -c "import json;d=json.load(open('$D/score.json'));print(d['pass'])")
ERRS=$(python3 -c "import json;d=json.load(open('$D/score.json'));print(d['errors'])")
[ -f "$ROOT/scores/log.md" ] || printf '| 時刻 | スコア | pass | エラー | commit | 計測 | メモ |\n| --- | ---: | --- | --- | --- | --- | --- |\n' > "$ROOT/scores/log.md"
echo "| $(date +%H:%M) | $SCORE | $PASS | $ERRS | $(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null) | $TS | $NOTE (raw=$(python3 -c "import json;print(json.load(open('$D/score.json'))['plus'])") conns=$CONNS bench=$BENCHCPU) |" >> "$ROOT/scores/log.md"
echo "===== $TS  SCORE=$SCORE pass=$PASS errors=$ERRS  ($NOTE)"
python3 -c "import json;d=json.load(open('$D/score.json'));[print('  ',m) for m in d['messages'][:12]];print('  table:',d['table'])"
for n in $NODES; do echo "--- isu$n busy=$(busy "$D/vmstat-isu$n.txt")"; head -3 "$D/cpu-isu$n.txt"; head -6 "$D/routes-isu$n.txt"; done
echo "--- bench busy=$BENCHCPU  max conns to isu1:443 = $CONNS  raw=$(python3 -c "import json;print(json.load(open('$D/score.json'))['plus'])")"
head -14 "$D/alp.txt"
