# CLAUDE.md — ISUCON12 予選 (isuports) 攻略の運用ルール

**ISUCON12 予選（マルチテナント SaaS「isuports」）で、レギュレーションを守った上で最高スコアを取る**ためのリポジトリ。
Claude Code が主体で「計測 → 改善」を回す。実装は **Go**（`webapp/go/`）。
汎用のナレッジは isucon-tools（`~/ghq/github.com/Dorayaki-World/isucon-tools/docs/`）。ここには**この問題固有のこと**だけ書く。

## 0. まず読む

| 知りたいこと | 場所 |
| --- | --- |
| 何をやったか | `docs/journal.md` |
| スコア推移 | `scores/log.md` |
| 計測の生データ | `measurements/<ts>/`（score.json → alp.txt → cpu-*.txt → vmstat-*.txt） |
| アプリ仕様 | `docs/reference/specification.md`（当日マニュアルは AMI に無い。要点は下） |
| 判断基準・実測して分かったこと | `.claude/skills/tuning-isucon12q/SKILL.md` |

## 1. レギュレーション要点（記憶ベース。原典マニュアルは未取得）

- 競技サーバーは c5.large × 3（`isucon12-qualify-1..3`）。`isucon12-qualify-4` は**ベンチ専用**（処理を載せない）
- ベンチは `https://<tenant>.t.isucon.local` を `isu1:443` に向けて叩く。`POST /initialize` は **30 秒以内**
- 整合性チェック失敗 / Critical Error は FAIL。エラーは件数に応じて減点（1% 単位）
- ランキングが 1.2 秒返らないと参加者 worker が離脱する
- 再起動後もデータが残っていること（追試は再起動 → 負荷走行）
- やっていい: テナント DB を SQLite 以外にする、スキーマ変更、ID 採番方式の変更、3 台の役割変更
- 変えてはいけない: URI・レスポンス JSON の構造、`public/` の中身、`/initialize` の `lang`

## 2. 大原則

isucon-tools の README「大原則」と同じ（計測なき改善は禁止 / 1 改善 = 1 コミット = 1 ベンチ / 下がったら revert /
サーバー上で直接編集しない / 計測記録は別コミット / 終盤は守り / ベンチはブラックボックス）。

## 3. 構成（`etc/isuN/services` が正）

```
bench(node4) ──443──> isu1: nginx stream (L4, ssl_preread)
                        ├─ admin.t.isucon.local ──> isu1 :8443 isuports（MySQL がローカル）
                        └─ それ以外 hash $ssl_preread_server_name consistent ──> isu2 / isu3 :8443 isuports
                      isu1: MySQL (tenant / tenant_billing だけ)
```

- **TLS は各ノードの Go が直接終端**（`front.go`。HTTP/1.1 のみ、RSA 署名は OpenSSL/cgo）。nginx は 443 を L4 で流すだけ。
  静的ファイルと `/auth/`（blackauth @isu1:3001）も Go が受ける
- **テナントのデータは各ノードのプロセス内メモリが正**、SQLite（`webapp/tenant_db/<id>.db`, WAL）は非同期 write-through の永続化
  （テナントごとに順序付きキュー。SIGTERM と initialize で書き切る）。テナント DB はノードローカルなので、nginx が SNI（=テナント）で担当ノードを固定する。
  **upstream の並び・weight を変えると担当が変わる**（initialize で全ノード初期化されるのでベンチ間では安全）
- `POST /initialize` を受けたノード（= isu1）が MySQL を初期化し、`ISUCON_PEERS` の他ノードに `:3000/internal/initialize` を投げる
- 初期データは変換済みの `/home/isucon/initial_data_v2/*.db`（`make migrate` が元データ + MySQL visit_history から一度だけ生成。20MB）
- 請求額は大会終了時に確定して SQLite `billing_report` と MySQL `tenant_billing` に書く。admin の請求一覧は MySQL だけで返す
- 計測は `curl localhost:3000/internal/stats?routes=1`（ルート別の件数と処理時間。`make bench` が `routes-isuN.txt` に保存）。alp は nginx が L4 なので使えない

## 4. 自律で進めてよい / 先に確認する

- よい: 計測、コード・設定変更、`make deploy` / `make bench` / `make restart-test`、revert、3 台の役割変更
- **先に確認**: 環境の破棄・作り直し、**ベンチ機のインスタンスタイプ変更**、レギュレーション解釈が割れる変更
- **不採用と決めたもの**: サーバー証明書の差し替え（ECDSA 化）。RSA 署名が CPU の 3〜4 割を占めるが、本番では運営配布の証明書なので使えない手

## 5. 環境

```sh
AWS_PROFILE=personal isuenv up isucon12-qualify --nodes 4 --ttl 6h   # 1..3 = 競技、4 = ベンチ
make setup && make deploy && make migrate && make bench
AWS_PROFILE=personal isuenv down isucon12-qualify
```

- ビルドはサーバー側（go-sqlite3 が CGO）。`/usr/local/go`（1.25.1）を `make setup` が入れる
- ベンチは約 80 秒。ベンチ機は c5.4xlarge に変更済み（c5.large だとベンチ機が先に飽和して 26 万点で頭打ち）。`ulimit -n`・
  `ip_local_port_range`・`tcp_tw_reuse` をベンチ機側で上げている（`tools/setup.sh`）。stop/start で public IP が変わったら `hosts.generated.mk` の `BENCH_IP` を直す
- MySQL root: `sudo mysql -uroot -proot`
