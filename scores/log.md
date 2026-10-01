| 時刻 | スコア | pass | エラー | commit | 計測 | メモ |
| --- | ---: | --- | --- | --- | --- | --- |
| 16:22 | 0 | False | 91(crit 1) | 4f6aa4c | 20261001-162246 | インメモリ化+ネイティブ実行 (1台) |
| 16:23 | 0 | False | 94(crit 1) | 8abb08d | 20261001-162313 | インメモリ化 (1台) bench の nofile 引き上げ |
| 16:24 | 256633 | True | 0(crit 0) | 8abb08d | 20261001-162336 | インメモリ化 (1台) bench nofile=1048576 |
| 16:29 | 259537 | True | 0(crit 0) | da9b581 | 20261001-162807 | 3台構成 (nginx hash $host, isu1=nginx+mysql+app, isu2/3=app) |
| 16:31 | 258491 | True | 0(crit 0) | 6882d74 | 20261001-162955 | 接続パターン計測用に nginx ログ列追加（コード同一＝ノイズ幅の確認） |
| 16:34 | 229415 | True | 11(crit 0) | b4809b1 | 20261001-163317 | 再起動試験 (3台 reboot 後) |
| 17:30 | 0 | False | 103(crit 1) | 3a37980 | 20261001-172901 | ベンチ機 c5.4xlarge に変更（コード同一） |
| 17:33 | 0 | False | 100(crit 0) | 580fdb7 | 20261001-173323 | TLS 直終端 (nginx stream SNI hash → 3台 :8443) |
| 17:34 | 0 | False | 285(crit 0) | 06e2403 | 20261001-173415 | TLS 直終端 + worker_connections 100000 |
| 17:36 | 828537 | True | 0(crit 0) | 4cc3f7c | 20261001-173518 | ベンチ機 port_range/tw_reuse 拡張 + pprof(isu2) |
| 17:40 | 34844 | True | 96(crit 0) | 91d5d3a | 20261001-173925 | JSON 手書き + rank 事前 JSON 化 + token key + GOGC=200 |
| 17:43 | 875796 | True | 0(crit 0) | 023ba67 | 20261001-174155 | 同一コード再計測（終了時 dial timeout の再現性確認）+ ベンチ機の接続数採取 |
| 17:45 | 802508 | True | 0(crit 0) | 08d0b24 | 20261001-174414 | stream weight 3:4:4 |
| 17:47 | 946636 | True | 0(crit 0) | 3c73c0b | 20261001-174643 | RSA 署名を OpenSSL(cgo) に + weight 均等に戻し |
| 17:51 | 975983 | True | 0(crit 0) | 68502fb | 20261001-174956 | player レスポンスキャッシュ + cookie 手パース |
| 17:53 | 972154 | True | 0(crit 0) | 470ea2b | 20261001-175203 | stream weight 1:2:2 |
