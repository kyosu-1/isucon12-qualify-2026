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

リクエスト成功数ベース（`score.json` の `table` が種類別の成功数）。参加者 worker は大会が作られるたびに増え、
ranking が 1.2 秒返らないと離脱する。admin の請求一覧を全ページ見終わるとテナントが 1 つ増える。

## 判断を間違えやすい点

1. **ベンチ機の CPU を先に見る**（`vmstat-bench.txt`）。インメモリ化後は c5.large のベンチ機が busy 93〜95% で、
   サーバーを速くしても点が動かない（1 台 256k → 3 台 259k、サーバー側レイテンシは 14ms → 2ms に下がったのに）
2. **請求額は大会終了時に確定**している。visit は「開催中の大会への初回閲覧」だけ記録。ここを触るなら整合性チェックを必ず通す
3. **テナントの担当ノードは nginx の hash で決まる**。アプリは全テナントをどのノードでも開けるが、同じテナントを 2 ノードで開いたらデータが割れる
4. メモリが正の読み取り元なので、**書き込みは必ず SQLite に commit してからメモリに反映**（再起動で復元できること）
5. initialize は 30 秒以内（現状 約 1 秒: 20MB のファイルコピー + 100 テナントのロード）

## 実測して分かったこと

| 観測 | 意味 | 根拠 |
| --- | --- | --- |
| 素の状態 2,033 点 | docker + go run、SQLite index なし、flock、visit_history 300 万行 | journal 16:11 |
| 初回の initialize が 30 秒超 | EBS のコールドスタート。2 回目から通る | journal 16:11 |
| インメモリ化で 256,633 点 | 読み取りは全部メモリ。SQLite は write-through | measurements/20261001-162336 |
| 速くなった途端ベンチが `too many open files` | ベンチ機の `ulimit -n` 1024。`tools/bench.sh` で hard limit まで上げる | measurements/20261001-162246 |
| 3 台にしても 259k | ベンチ機 busy 95% が上限。isu1 52% / isu2,3 15% | measurements/20261001-162807 |
| 同一コードのブレ ±0.5% | 258,491 / 259,537（ベンチ機律速のため小さい） | scores/log.md |
| 接続 14,616 本 / 21 万リクエスト、TLS セッション再利用 0、全部 HTTP/2 | ベンチは参加者ごとに新規 TLS 接続 | measurements/20261001-162955 |

### 踏んだ罠

- `rsync --exclude=isuports` が `cmd/isuports/` まで除外して古い main.go のままビルドされた → `--exclude=/isuports`
- MySQL の `bind-address` は `mysql.conf.d/mysqld.cnf` が後勝ち。`conf.d/` に置いた設定は上書きされる → `mysql.conf.d/zz-isucon.cnf`
- docker 版の unit を置き換えた直後、古いコンテナが :3000 を掴んだままで新バイナリが bind に失敗 → docker を disable --now

## 締め

1. `make restart-test` 2. slow log が OFF（この問題では一度も ON にしていない）3. 新しい大きい変更をしない
