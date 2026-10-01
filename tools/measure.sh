#!/bin/bash
# slow query log の ON/OFF（MySQL が動いている全ノード）
set -u; . "$(dirname "$0")/env.sh"
case "${1:-}" in
  on)  Q="SET PERSIST slow_query_log_file='/var/log/mysql/slow.log'; SET PERSIST long_query_time=0; SET PERSIST slow_query_log=1;" ;;
  off) Q="SET PERSIST slow_query_log=0;" ;;
  *) echo "usage: measure.sh on|off"; exit 1 ;;
esac
for n in $NODES; do
  grep -qx mysql "$ROOT/etc/isu$n/services" 2>/dev/null || continue
  $SSH $(host_of $n) "sudo mysql -uroot -proot -e \"$Q SELECT @@slow_query_log, @@long_query_time\" 2>/dev/null; systemctl is-active -q isuports && sudo systemctl restart isuports" | sed "s/^/[isu$n] /"
done
