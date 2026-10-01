---
name: tuning-isucon12q
description: Use when tuning ISUCON12 qualify (isuports) for a benchmark score - running make bench, reading measurements/, choosing the next bottleneck, changing tenant store / ranking / billing logic, or deciding whether to keep or revert a change.
---

# ISUCON12 予選 (isuports) 計測改善ループ

**計測なき改善は禁止。1改善 = 1コミット = 1ベンチ。** 汎用の定石は `isucon-tools/docs/`。ここは固有の話だけ。

```sh
make bench NOTE="何を確かめたいか"
# score.json（pass / errors / table）→ alp.txt → cpu-isuN.txt → vmstat-bench.txt
make deploy && make bench
git add measurements scores docs && git commit -m "bench: <score> <何の計測か>"
```

## スコアの構造

リクエスト成功数ベース（`score.json` の `table` が種類別の成功数、bench.log の `ScenarioScoreMap` がシナリオ別の点）。
- **Player**（全体の約 7 割）: 参加者 worker は大会が作られるたびに増え、1〜3 秒おきに 1 リクエスト投げる。ranking が 1.2 秒返らないと離脱
- **OrganizerNewTenant**（約 3 割）: テナント数に比例。テナントは「admin が請求一覧を全ページ見終わるたびに 1 つ」増えるので、admin billing の速さで決まる
- エラーは 1 件につき 1% 減点、100 件超で FAIL

## 判断を間違えやすい点

1. **ベンチ機を先に見る**（`vmstat-bench.txt`、`scores/log.md` の `conns=` と `bench=`）。この環境の上限はベンチ機:
   - c5.large では busy 95% で 26 万点止まり → c5.4xlarge
   - ベンチ機 → isu1:443 の同時接続が約 3.2 万（片方の偶奇のエフェメラルポート数）を超えると connect() が遅くなり、
     負荷終了時刻ちょうどに dial 中だったリクエストが `dial tcp ... i/o timeout` でエラー計上される（0 件の回と数十〜数百件の回がある）
2. **スコアだけでなく raw（減点前）と conns を見る**。同じ構成でも 119 万点の回と FAIL の回がある。raw が同じなら構成の差ではなくベンチ機の当たり外れ
3. **サーバーを速くすると参加者が増えて接続が増え、かえって減点が増える**ことがある（RSA 署名を 1 並列にしたらレイテンシは 11.9ms → 0.32ms、raw 129 万だが dial timeout 235 件で FAIL）
4. **請求額は大会終了時に確定**している。visit は「開催中の大会への初回閲覧」だけ記録。ここを触るなら整合性チェックを必ず通す
5. **テナントの担当ノードは nginx の SNI ハッシュで決まる**。同じテナントを 2 ノードで開いたらデータが割れる
6. メモリが正の読み取り元。SQLite への書き込みは非同期だが**順序付きキュー**で、停止時に書き切る（再起動で復元できること。`make restart-test`）
7. 2 vCPU は同じ物理コアの HT。RSA 署名は 2 並列にしても伸びない（1677/s → 1766/s）。pidstat の % は足し算できない

## 実測して分かったこと

| 観測 | 意味 | 根拠 |
| --- | --- | --- |
| 素の状態 2,033 点 | docker + go run、SQLite index なし、flock、visit_history 300 万行 | journal 16:11 |
| インメモリ化で 256,633 点 | 読み取りは全部メモリ | measurements/20261001-162336 |
| 3 台にしても 259k、ベンチ機 busy 95% | ベンチ機が律速 | measurements/20261001-162807 |
| ベンチ機を 16vCPU にすると isu1 が busy 94%（nginx 136%）で FAIL | TLS/HTTP2/proxy を 1 台に集めると詰まる | measurements/20261001-172901 |
| TLS を各ノードの Go で終端（nginx は L4）→ 828,537 | | measurements/20261001-173518 |
| RSA 署名 Go 標準 1.37ms → OpenSSL 0.60ms | 946,636 | measurements/20261001-174643、`isuports benchsign` |
| HTTP/2 → HTTP/1.1 で ranking 3.7ms → 0.24ms | Go の h2 サーバーは 1 レスポンスで goroutine を何度も渡り歩く。1,105,386 | measurements/20261001-181715 |
| score 入稿 18ms は全部リクエストボディ待ち | ベンチ側の時間。SQLite を非同期にしても変わらない | measurements/20261001-182630 |
| isu1 で TCP メモリ圧迫（RcvQDrop 59 万） | nginx stream のバッファが接続あたり 50KB。4k/8k に縮小 + tcp_mem 拡張 | measurements/20261001-183308 |
| isu1 を L4 + MySQL + admin 専任に → 1,175,915 / 1,197,784 | admin billing 1.9ms → 0.56ms、テナント 284 → 574 | measurements/20261001-184052, -185121 |
| アイドル 5 秒で接続を閉じる → FAIL（crit 5） | 接続はほぼ全て生きている参加者（間隔 1〜3 秒が 70%）。間引けない | measurements/20261001-185719, -190025 |
| 終了時 dial timeout: isu1 への connect は p99 0.45ms、ListenDrops 0 | サーバーは SYN を落としていない。ベンチ機側のポート空間が原因 | journal「ベンチ終了時の dial tcp」 |

### 踏んだ罠

- `rsync --exclude=isuports` が `cmd/isuports/` まで除外して古い main.go のままビルドされた → `--exclude=/isuports`
- MySQL の `bind-address` は `mysql.conf.d/mysqld.cnf` が後勝ち。`conf.d/` に置いた設定は上書きされる → `mysql.conf.d/zz-isucon.cnf`
- docker 版の unit を置き換えた直後、古いコンテナが :3000 を掴んだままで新バイナリが bind に失敗 → docker を disable --now
- `sites-available/default` の `listen 443 ssl` が stream の 443 と衝突（`nginx -t` は通るのに起動で落ちる）
- nginx stream は 1 接続 2 スロット。`worker_connections` 8192 は 16 秒で枯渇
- ベンチ機のエフェメラルポート枯渇（`cannot assign requested address`）。`tcp_tw_reuse=2 + tcp_max_tw_buckets=4096` は SYN challenge が増えて逆効果
- ビルド失敗に気づかず旧バイナリでベンチした（スコアが妙に同じなら `make deploy` の出力を見る）
- nginx stream の weight を変えるとハッシュの割り当てが変わり、スコアが 10% 動く（3:4:4 で 875k → 802k）

## 締め

1. `make restart-test` 2. slow log が OFF（この問題では一度も ON にしていない）3. 新しい大きい変更をしない 4. 最終計測は複数回（終了時 dial timeout の当たり外れがある）
