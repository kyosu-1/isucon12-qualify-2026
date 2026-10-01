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
