#!/bin/bash
# 全台 reboot → boot_id が変わるまで待つ → enabled なサービスの is-active → API 応答待ち → ベンチ
set -u; . "$(dirname "$0")/env.sh"
declare -a OLD
for n in $NODES; do OLD[$n]=$($SSH $(host_of $n) cat /proc/sys/kernel/random/boot_id); done
for n in $NODES; do $SSH $(host_of $n) 'sudo systemctl reboot' >/dev/null 2>&1 & done; wait
for n in $NODES; do
  h=$(host_of $n)
  for i in $(seq 1 60); do
    new=$($SSH -o ConnectTimeout=5 $h cat /proc/sys/kernel/random/boot_id 2>/dev/null || true)
    [ -n "$new" ] && [ "$new" != "${OLD[$n]}" ] && break; sleep 5
  done
  $SSH $h 'systemctl is-system-running --wait >/dev/null 2>&1; for s in '"$(tr '\n' ' ' < "$ROOT/etc/isu$n/services")"'; do printf "%s=%s/%s " $s "$(systemctl is-enabled $s)" "$(systemctl is-active $s)"; done; echo' | sed "s/^/[isu$n] /"
done
for i in $(seq 1 30); do
  code=$($SSH $ISU1 'curl -sk -o /dev/null -w "%{http_code}" --resolve admin.t.isucon.local:443:127.0.0.1 https://admin.t.isucon.local/api/me' 2>/dev/null)
  [ "$code" = 200 ] && break; sleep 3
done
echo "API: $code"
# 再起動後にデータが残っていること（initialize を挟まずに確認）: 直前ベンチで作られたテナント数
$SSH $ISU1 'mysql -uisucon -pisucon isuports -N -e "SELECT COUNT(*), MAX(id) FROM tenant" 2>/dev/null' | sed 's/^/tenant count,max: /'
NOTE="${NOTE:-再起動試験}" bash "$ROOT/tools/bench.sh"
