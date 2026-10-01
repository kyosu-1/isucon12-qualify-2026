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
  # tcp_tw_reuse=1 は使わない: 5.19 カーネルは FIN_WAIT2 の tw ソケットのポートも 1 秒で再利用するので、サーバー側がまだ閉じていない
  # 接続と同じ 4-tuple で SYN を送ってしまい、challenge ACK → RST → SYN 再送 1 秒待ちになる（isu1 で TCPSYNChallenge 100〜500/回。
  # 負荷終了の瞬間に dial 中だった ranking が i/o timeout としてエラー計上され、1 件 1% の減点になっていた）。
  # 代わりに TIME_WAIT を持たせる数を絞ってポートを早く空ける（ポートは巡回して割り当てられるので、再利用は数十秒後になる）
  printf "net.ipv4.ip_local_port_range = 1024 65535\nnet.ipv4.tcp_tw_reuse = 2\nnet.ipv4.tcp_max_tw_buckets = 4096\n" | sudo tee /etc/sysctl.d/99-bench.conf >/dev/null; sudo sysctl --system >/dev/null 2>&1; sysctl net.ipv4.ip_local_port_range net.ipv4.tcp_tw_reuse net.ipv4.tcp_max_tw_buckets' &
wait
