#!/bin/bash
# 初期データの変換（一回限り。isu1 で実行して initial_data_v2 を他ノードに配る）
set -u; . "$(dirname "$0")/env.sh"
$SSH $ISU1 'sudo -u isucon bash -c "cd /home/isucon/webapp/go && set -a && . /home/isucon/env.sh && set +a && ISUCON_DB_HOST=127.0.0.1 ./isuports migrate" | tail -3; sudo du -sh /home/isucon/initial_data_v2'
for n in $NODES; do
  [ "$n" = 1 ] && continue
  h=$(host_of $n)
  $SSH $ISU1 "sudo tar cf - -C /home/isucon initial_data_v2" | $SSH $h "sudo tar xf - -C /home/isucon && sudo chown -R isucon:isucon /home/isucon/initial_data_v2 && sudo du -sh /home/isucon/initial_data_v2" | sed "s/^/[isu$n] /"
done
