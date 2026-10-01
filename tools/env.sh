# 共通設定。source して使う
SSH="ssh -o LogLevel=ERROR"
ISU1=isucon12-qualify-1
ISU2=isucon12-qualify-2
ISU3=isucon12-qualify-3
BENCH=isucon12-qualify-4
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
[ -f "$ROOT/hosts.generated.mk" ] || {
  for n in 1 2 3; do h=isucon12-qualify-$n; echo "ISU${n}_IP=$($SSH $h "hostname -I | cut -d' ' -f1")"; done > "$ROOT/hosts.generated.mk"
}
. "$ROOT/hosts.generated.mk"
# ベンチ機を stop/start でタイプ変更すると public IP が変わり isuenv の ssh 設定が古くなる。
# hosts.generated.mk に BENCH_IP=<public ip> があればそちらを使う
[ -n "${BENCH_IP:-}" ] && BENCH="-i $HOME/.ssh/isuenv.pem -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null ubuntu@$BENCH_IP"
# etc/isuN/ があるノード = 管理対象
NODES=""; for n in 1 2 3; do [ -d "$ROOT/etc/isu$n" ] && NODES="$NODES $n"; done
host_of() { echo "isucon12-qualify-$1"; }
