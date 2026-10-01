# ISUCON12 予選 (isuports) 練習 — 2026-10-01

matsuu/aws-isucon の AMI を isuenv で立て（競技 c5.large × 3 + ベンチ 1）、Claude Code が計測 → 改善のループを回した記録。
進め方と道具立ては [isucon-tools](https://github.com/Dorayaki-World/isucon-tools) に沿っている。

## 到達点

| | スコア | 条件 |
| --- | ---: | --- |
| 素の状態 | 2,033 | docker + go run、SQLite に index なし |
| ベンチ機 c5.large のまま | 259,537 | ベンチ機が busy 95% で頭打ち |
| **最終構成（3 台均等）** | **約 1,030,000** | ベンチ機 c5.4xlarge。pass・エラー 0 で安定（`scores/log.md` の「3 台均等」の行） |
| 最高点（isu1 専任構成） | **1,197,784** | pass・エラー 0。ただし同じ構成の 5 連続計測は 923,295 / FAIL / FAIL / 1,168,611 / 1,191,223 |

最高点の構成は raw（減点前）が毎回約 120 万だが、ベンチ機側の事情（下記）で負荷終了時に `dial tcp ... i/o timeout` が 0〜1000 件出て、
1 件 1% の減点・100 件超で FAIL になる。**最終状態は安定して通る 3 台均等の構成にしてある。**
最高点の構成に戻すには `git log --oneline -- etc/isu1/nginx/nginx.conf` の「isu1 = L4 中継 + MySQL + admin API 専任」のコミットの nginx.conf を使う。

## 構成

```
bench ──443──> isu1: nginx stream (L4, SNI ハッシュ) ──> isu1 / isu2 / isu3 :8443  isuports (Go が TLS 終端・HTTP/1.1)
               isu1: MySQL（tenant / tenant_billing だけ）
```

- テナントのデータは各ノードのプロセス内メモリが正。SQLite は非同期 write-through の永続化（停止時と initialize で書き切る）
- 請求額は大会終了時に確定。初期データ分は `isuports migrate` が MySQL の visit_history 300 万行から畳み込む（270MB → 20MB）
- TLS ハンドシェイクの RSA 署名は OpenSSL (cgo)。証明書は AMI のまま（ECDSA への差し替えは本番で使えない手なので不採用）

## 効いた順

| 変更 | スコア |
| --- | ---: |
| インメモリ化 + docker をやめてネイティブ実行 + 請求の事前確定 + ID 採番をプロセス内に | 2,033 → 256,633 |
| ベンチ機を c5.4xlarge に + TLS を各ノードの Go で直接終端（nginx は L4） | → 828,537 |
| JSON 手書き / ランキングの事前 JSON 化 / JWT キャッシュ | → 875,796 |
| RSA 署名を OpenSSL に（1.37ms → 0.60ms） | → 946,636 |
| player レスポンスのキャッシュ | → 975,983 |
| HTTP/2 をやめて HTTP/1.1（ranking 3.7ms → 0.24ms） | → 1,105,386 |
| isu1 を L4 + MySQL + admin 専任に（テナント 284 → 574） | → 1,197,784（不安定） |

効かなかった / 戻したもの: nginx stream の weight 調整、visit_history・SQLite 書き込みの非同期化（害は無いので残した）、
`DynamicRecordSizingDisabled`、RSA 署名の 1 並列化（レイテンシは激減するが FAIL）、アイドル接続の切断（FAIL）。詳細は `docs/journal.md`。

## この環境の上限はベンチ機

- c5.large のベンチ機は 26 万点で CPU が飽和する
- c5.4xlarge でも、ベンチ機 → isu1:443 の同時接続が約 3.2 万（片方の偶奇のエフェメラルポート数）を超えると connect() のポート探索が遅くなり
  （ベンチ機 busy 48% → 66%）、負荷終了時刻に dial 中のリクエストがエラー計上される。サーバー側は SYN を落としていない（connect p99 0.45ms、ListenDrops 0）
- テナント作成が速くなると、ベンチが同じ秒に同じテナント名を生成して `tenants/add` が 400（重複）→ Critical Error になることがある

## 再現

```sh
AWS_PROFILE=personal isuenv up isucon12-qualify --nodes 4 --ttl 6h
# ベンチ機 (node4) を c5.4xlarge に変えたら hosts.generated.mk に BENCH_IP=<public ip>
make setup && make deploy && make migrate && make bench NOTE="..."
make restart-test
```

記録: `scores/log.md`（全ベンチ）、`measurements/<ts>/`（生データ）、`docs/journal.md`（時系列）、`.claude/skills/tuning-isucon12q/SKILL.md`（判断基準）。
