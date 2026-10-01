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
