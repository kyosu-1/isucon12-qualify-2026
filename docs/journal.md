# journal — ISUCON12 予選 (isuports) 2026-10-01

環境: `AWS_PROFILE=personal isuenv up isucon12-qualify --nodes 4 --ttl 6h`（16:05 JST 起動 → 22:05 自動破棄）
isu1〜3 = 競技ノード（c5.large 2vCPU/3.6GB）、node4 = ベンチ機（同 AMI、競技サービスは disable）。

### 16:08 サーバー調査
- Ubuntu 22.04 / MySQL 8 (127.0.0.1:3306) / nginx 443 http2 / アプリは `docker compose up --build` で go run（isuports.service）/ blackauth :3001
- MySQL `isuports`: tenant 100 行、visit_history 約 300 万行 (210MB, index は tenant_id のみ)、id_generator
- テナント DB は SQLite（`webapp/tenant_db/<id>.db`、1.db だけ 126MB、合計 270MB）。index なし。flock で排他
- ベンチは `/home/isucon/bench/bench -target-url https://t.isucon.local -target-addr <isu1>:443`（ベンチのソースは読まない）
- MySQL root は `sudo mysql -uroot -proot`

### 16:11 素の状態でベンチ（手動）
- 1 回目: initialize が 30s タイムアウト（EBS のコールドスタート）。2 回目: **SCORE 2033**（エラー 1: 終了間際の ranking decode 失敗）

### 16:15 道具立て
`tools/`（env / setup / deploy / bench / parse.py / measure / migrate / pprof / restart-test）と `Makefile` を作成。
ビルドは go-sqlite3 が CGO のためサーバー側（Go 1.25.1 を /usr/local/go に導入）。

### 16:20 インメモリストア化（一括書き換え）
この問題は構造が既知なので、個別に index を足す前にデータの持ち方を変えた（`webapp/go/store.go`, `migrate.go`）。
- テナントのデータ（player / competition / score / 開催中大会の閲覧者）をプロセス内メモリに持ち、SQLite は WAL の write-through 永続化
- flock → テナント単位の RWMutex。player_score は参加者ごとに最終行だけ保持。ランキングは入稿時にソートしてキャッシュ
- 請求額は大会終了時に確定（SQLite `billing_report` + MySQL `tenant_billing`）。初期データ分は `isuports migrate` が MySQL visit_history から畳み込む（270MB → 20MB、37 秒）
- ID 採番は起動時刻 µs + カウンタ + ノード番号（MySQL の REPLACE をやめた）。JWT は鍵を起動時に読み、検証済みトークンをキャッシュ
- docker compose + go run をやめて systemd から直接バイナリ実行
ハマり: rsync の exclude が cmd/isuports を巻き込む / 旧コンテナが :3000 を掴む / ベンチ機の ulimit 1024 で FAIL（SCORE 0 が 2 回）
結果: **256,633**（pass、エラー 0）。isu1 busy 72%（nginx 73% + app 53%）、**ベンチ機 busy 95%**

### 16:27 3 台構成
nginx `hash $host consistent` でテナントごとに担当ノードを固定、MySQL は isu1。initialize は peers に伝播。
結果: 259,537 / 258,491。サーバー側は isu1 52%・isu2/3 15% とガラ空きだがスコアは動かない = **ベンチ機が律速**。
接続は 14,616 本・TLS 再利用 0・全部 HTTP/2（参加者ごとに新規接続）。

### 16:33 再起動試験（3 台 reboot → ベンチ）
`make restart-test`: 全サービス enabled/active、再起動後も前回ベンチのテナントが残っている（tenant 186 行）。
結果 229,415（pass、エラー 11）。エラーは全部ベンチ側の `dial tcp ...:443: i/o timeout` が同時刻に 11 件。
isu1 の ListenOverflows/ListenDrops は 0、ベンチ機の TCPSynRetrans も 0 → サーバーが落としたのではなく、busy 94% のベンチ機自身の取りこぼしと判断。

### 17:25 ベンチ機を c5.4xlarge に変更（ユーザー承認済み）
c5.large のベンチ機が busy 95% で頭打ちだったため、node4 を stop → c5.4xlarge → start。public IP が変わるので `hosts.generated.mk` に `BENCH_IP=` を書いて
`tools/env.sh` がそちらを使う。TTL（cron で自己 shutdown）は stop/start 後も有効。**以降のスコアは 16:36 までの数字と直接比較できない。**
- 同一コードで FAIL: isu1 busy 94%（nginx 136% + app 32%）、isu2/3 は 17%。p99 1.8s、499 が 3502、connection reset 135 件

### 17:33 TLS を各アプリノードで直接終端（nginx は L4 stream）
isu1 の nginx は 443 を `stream` + `ssl_preread`（SNI ハッシュ）で 3 台の :8443 に流すだけにし、Go が TLS を終端（`front.go`）。静的ファイルと `/auth/` も Go で受ける。
- ハマり 1: `sites-available/default` が `listen 443 ssl default_server` を持っていて stream の 443 と衝突（`nginx -t` は通るが起動で bind 失敗）
- ハマり 2: stream は 1 接続 2 スロット。`worker_connections 8192` を 16 秒で使い切り EOF 100 件 → 100000
- ハマり 3: ベンチ機のエフェメラルポート枯渇（`cannot assign requested address`）→ ベンチ機に `ip_local_port_range=1024 65535` / `tcp_tw_reuse=1`
結果: **828,537**（pass、エラー 0）。pprof: RSA 署名 28% / JSON 15% / h2 11%

### 17:40〜17:50 アプリの CPU 削減
- JSON 手書き（player / ranking / competitions）、ランキングは入稿時に JSON 化、JWT キャッシュのキー短縮、GOGC=200 → 875,796
- nginx stream の weight 3:4:4 → 802,508（下がったので revert）
- RSA 署名を OpenSSL (cgo) に（`opensslsign.go`。Go 標準 1.37ms → OpenSSL 0.60ms/sign）→ 946,636
- player レスポンスのキャッシュ（テナントの scoreVer 単位）+ Cookie 手パース → 975,983
- `/internal/stats`（テナント別リクエスト数、ルート別処理時間）を追加。nginx を L4 にして alp が使えなくなった代わり

### 18:00 証明書の差し替えは不採用（ユーザー判断）
RSA 署名が app CPU の約 30〜40%。自己署名 ECDSA 証明書に差し替えれば消えるが、本番では運営配布の証明書なので使えない手。**差し替えない**と決定。

### 18:17 HTTP/2 をやめて HTTP/1.1 で受ける
ルート別計測で ranking が avg 3.7ms（書き出し前は 0.29ms）。Go の h2 サーバーはフレームごとに goroutine を渡り歩くので、CPU が詰まると書き出しが遅れる。
ALPN で h2 を広告しない（`ISUCON_HTTP2=0`）→ **1,105,386**。ranking avg 0.24ms。
- 効かなかった: visit_history の非同期化（原因は SQLite ではなかった）、`DynamicRecordSizingDisabled`、SQLite 書き込み全体の非同期キュー化（score 入稿 18ms は「リクエストボディの到着待ち」= ベンチ側だった）。
  非同期キューは害が無いので残した（停止時 SIGTERM と initialize で書き切る）
- 事故: パッチで `store.go` の後半を消してビルド失敗 → 旧バイナリのままベンチしていた（129,418 の行）。deploy のビルド失敗表示を目立たせた

### 18:30 isu1 の TCP メモリ圧迫
`nstat` で isu1 に TCPMemoryPressures 12 回 / PruneCalled 59 万 / TCPRcvQDrop 59 万 / TCPTimeouts 9.3 万。nginx worker の RSS が 800MB × 2
（stream の preread 16k + proxy 16k×2 が接続ごと）。`preread_buffer_size 4k` / `proxy_buffer_size 8k`、`tcp_mem` 拡張 → 圧迫 0、nginx RSS 340MB。スコアは変わらず 1,075,003

### 18:40 isu1 を L4 中継 + MySQL + admin API 専任に
テナントは isu2 / isu3 だけに載せ、`admin.t.isucon.local` は isu1 のアプリ（MySQL がローカル）に固定（nginx `map`）。
admin billing が avg 1.9ms → 0.56ms、7k → 23k 回、新規テナント 284 → 574。**1,175,915**（エラー 1）、その後 **1,197,784**（エラー 0）。

### 18:45 RSA 署名の同時実行を 1 本に絞る → 不採用
レイテンシは劇的に良くなる（player avg 11.9ms → 0.32ms、raw 129 万）が、dial timeout 235 件で FAIL。`ISUCON_SIGN_PARALLEL` で指定可能な形にして既定は無制限に戻した。

### ベンチ終了時の `dial tcp ... i/o timeout`（1 件 1% 減点）について分かったこと
- 負荷走行の終了時刻（開始 +60.0 秒）ちょうどに、dial 中だったリクエストがまとめてエラー計上される。0 件の回と数十〜数百件の回がある
- isu3 から isu1:443 への TCP connect は負荷中も p50 0.28ms / p99 0.45ms。isu1 の ListenOverflows / ListenDrops は常に 0 → **サーバーは SYN を落としていない**
- 原因 A: ベンチ機の `tcp_tw_reuse=1` が FIN_WAIT2 のポートも 1 秒で再利用し、サーバー側がまだ閉じていない 4-tuple に SYN → challenge ACK → SYN 再送 1 秒
  （isu1 `TCPSYNChallenge` = ベンチ機 `TCPSynRetrans`）。`tw_reuse=2 + tcp_max_tw_buckets=4096` はさらに悪化（4712 回）したので戻した
- 原因 B: ベンチ機 → isu1:443 の同時接続が 3.2 万（片方の偶奇のエフェメラルポート数）を超えると、ベンチ機の connect() がポート探索で遅くなる
  （ベンチ機 busy 48% → 66%、同時接続 4.3〜4.4 万）。dial が滞留し、終了時刻に引っかかる数が増える
- つまり **この環境の上限はベンチ機 1 台・宛先 1 つのポート空間**。サーバーを速くするほど参加者が増えて接続が増え、ここに当たる
