#!/bin/bash
# 唯一の反映経路: rsync → サーバーで go build (CGO/sqlite3) → /etc 反映 → 差分のあるものだけ restart → 疎通確認
set -u; . "$(dirname "$0")/env.sh"
[ -n "$(git -C "$ROOT" status --porcelain -- webapp etc)" ] && echo "WARN: 未コミットの変更があります"
RS="rsync -az -e 'ssh -o LogLevel=ERROR'"
deploy_node() {
  local n=$1 h; h=$(host_of $n); local E="$ROOT/etc/isu$n" T; T=$(mktemp -d)
  # etc は IP プレースホルダを置換した一時コピーを配る
  cp -R "$E/." "$T/"; rm -rf "$T/nginx/tls"
  grep -rlE '__ISU[123]_IP__' "$T" 2>/dev/null | while read -r f; do
    sed -i '' -e "s/__ISU1_IP__/$ISU1_IP/g; s/__ISU2_IP__/$ISU2_IP/g; s/__ISU3_IP__/$ISU3_IP/g" "$f"
  done
  rsync -az -e 'ssh -o LogLevel=ERROR' --rsync-path='sudo -u isucon rsync' --delete --exclude=/isuports --exclude=/isuports.new "$ROOT/webapp/go/" "$h:/home/isucon/webapp/go/" || return 1
  rsync -az -e 'ssh -o LogLevel=ERROR' --rsync-path='sudo -u isucon rsync' --exclude=90_data.sql "$ROOT/webapp/sql/" "$h:/home/isucon/webapp/sql/" || return 1
  rsync -az -e 'ssh -o LogLevel=ERROR' --delete "$T/" "$h:/tmp/etc-deploy/" || return 1
  rm -rf "$T"
  $SSH $h '
    set -u; D=/tmp/etc-deploy; SV=$(cat $D/services 2>/dev/null)
    has() { echo "$SV" | grep -qx "$1"; }
    sync_file() { # src dst -> 差分があれば置いて 0 を返す
      [ -f "$1" ] || return 1
      sudo cmp -s "$1" "$2" && return 1
      sudo install -m 644 "$1" "$2"; return 0
    }
    sudo -u isucon bash -c "cd /home/isucon/webapp/go && /usr/local/go/bin/go build -o isuports.new ./cmd/isuports && mv isuports.new isuports" || { echo "BUILD FAILED"; exit 1; }
    sudo install -o isucon -g isucon -m 644 $D/home/env.sh /home/isucon/env.sh
    sync_file $D/systemd/isuports.service /etc/systemd/system/isuports.service && sudo systemctl daemon-reload
    if [ -f $D/sysctl.d/99-isucon.conf ] && sync_file $D/sysctl.d/99-isucon.conf /etc/sysctl.d/99-isucon.conf; then sudo sysctl --system >/dev/null 2>&1 || true; fi
    sudo systemctl disable --now docker docker.socket containerd redis-server >/dev/null 2>&1
    for s in mysql nginx isuports; do
      if has $s; then sudo systemctl enable $s >/dev/null 2>&1; else sudo systemctl disable --now $s >/dev/null 2>&1; fi
    done
    if has mysql; then
      if sudo rm -f /etc/mysql/conf.d/zz-isucon.cnf; sync_file $D/mysql/mysql.conf.d/zz-isucon.cnf /etc/mysql/mysql.conf.d/zz-isucon.cnf; then echo "mysql: restart"; sudo systemctl restart mysql; else sudo systemctl start mysql; fi
    fi
    if has nginx; then
      ch=0; sudo systemctl is-active -q blackauth || sudo systemctl enable --now blackauth >/dev/null 2>&1
      sync_file $D/nginx/nginx.conf /etc/nginx/nginx.conf && ch=1
      sync_file $D/nginx/sites-available/isuports.conf /etc/nginx/sites-available/isuports.conf && ch=1
      sudo nginx -t 2>&1 | grep -v "syntax is ok" | grep -v "test is successful"
      sudo nginx -t >/dev/null 2>&1 || { echo "NGINX CONFIG NG"; exit 1; }
      if [ $ch = 1 ]; then echo "nginx: restart"; sudo systemctl restart nginx; else sudo systemctl start nginx; fi
    fi
    if has isuports; then sudo systemctl restart isuports; fi
    sleep 1
    for s in $SV; do printf "%s=%s " $s "$(systemctl is-active $s)"; done; echo
  ' 2>&1 | sed "s/^/[isu$n] /"
}
# DB ノード（mysql を持つノード）を先に、残りは並列
for n in $NODES; do grep -qx mysql "$ROOT/etc/isu$n/services" 2>/dev/null && deploy_node $n; done
for n in $NODES; do grep -qx mysql "$ROOT/etc/isu$n/services" 2>/dev/null || deploy_node $n & done
wait
$SSH $ISU1 'curl -sk -o /dev/null -w "GET /api/me -> %{http_code} (%{time_total}s)\n" --resolve admin.t.isucon.local:443:127.0.0.1 https://admin.t.isucon.local/api/me'
