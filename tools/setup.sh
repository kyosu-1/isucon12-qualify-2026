#!/bin/bash
# 計測ツールと Go を入れる（冪等）。ベンチ機の競技サービスを止める
set -u; . "$(dirname "$0")/env.sh"
GOVER=1.25.1
for n in 1 2 3; do
  $SSH $(host_of $n) "
    sudo systemctl disable --now unattended-upgrades apt-daily.timer apt-daily-upgrade.timer >/dev/null 2>&1
    command -v alp >/dev/null || { curl -fsSL https://github.com/tkuchiki/alp/releases/download/v1.0.21/alp_linux_amd64.tar.gz | tar xz alp && sudo install alp /usr/local/bin/ && rm alp; }
    command -v pt-query-digest >/dev/null && command -v pidstat >/dev/null || { sudo apt-get update -qq >/dev/null; sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq percona-toolkit sysstat >/dev/null; }
    [ -x /usr/local/go/bin/go ] || { curl -fsSL https://go.dev/dl/go$GOVER.linux-amd64.tar.gz | sudo tar xz -C /usr/local; }
    dpkg -s libssl-dev >/dev/null 2>&1 || { sudo apt-get update -qq >/dev/null; sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq libssl-dev >/dev/null; }
    /usr/local/go/bin/go version; alp --version; pidstat -V | head -1
  " 2>&1 | sed "s/^/[isu$n] /" &
done
$SSH $BENCH 'sudo systemctl disable --now isuports nginx mysql redis-server blackauth unattended-upgrades apt-daily.timer apt-daily-upgrade.timer >/dev/null 2>&1; echo "[bench] services stopped"
  # ベンチは参加者ごとに新規 TLS 接続を張っては閉じる。既定のポート範囲 (約 2.8 万) だと TIME_WAIT で枯渇して
  # connect: cannot assign requested address になる（サーバーが速くなると 35 秒で発生）
# tcp_tw_reuse=1: TIME_WAIT のポートを 1 秒後から再利用する。これが無いと 1 回のベンチ（約 6 万接続 + 常時 3 万接続）でポートが枯渇する。
  # 副作用: サーバー側がまだ閉じていない接続と同じ 4-tuple で SYN を送ることがあり（isu1 で TCPSYNChallenge 約 100/回）、
  # その dial は SYN 再送で 1 秒待つ。負荷終了の瞬間に dial 中だと i/o timeout としてエラー計上される（1 件 1% 減点）。
  # tw_reuse=2 + tcp_max_tw_buckets=4096（TIME_WAIT を持たせない）は閉じた直後のポートを再利用して SYNChallenge が 4000 超に悪化した
  printf "net.ipv4.ip_local_port_range = 1024 65535\nnet.ipv4.tcp_tw_reuse = 1\nnet.ipv4.tcp_max_tw_buckets = 262144\n" | sudo tee /etc/sysctl.d/99-bench.conf >/dev/null; sudo sysctl --system >/dev/null 2>&1; sysctl net.ipv4.ip_local_port_range net.ipv4.tcp_tw_reuse net.ipv4.tcp_max_tw_buckets' &
wait
