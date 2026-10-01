.PHONY: help setup deploy bench measure-on measure-off migrate pprof logs info restart-test
help:
	@echo "setup / deploy / bench NOTE=... / measure-on / measure-off / migrate / pprof / logs / info / restart-test"
setup:        ; bash tools/setup.sh
deploy:       ; bash tools/deploy.sh
bench:        ; NOTE="$(NOTE)" bash tools/bench.sh
measure-on:   ; bash tools/measure.sh on
measure-off:  ; bash tools/measure.sh off
migrate:      ; bash tools/migrate.sh
pprof:        ; bash tools/pprof.sh $(N)
logs:         ; ssh -o LogLevel=ERROR isucon12-qualify-$(or $(N),1) 'sudo journalctl -u isuports --no-pager -n 60 -o cat'
info:         ; @bash -c '. tools/env.sh; echo "nodes:$$NODES"; cat hosts.generated.mk; for n in $$NODES; do echo "isu$$n: $$(tr "\n" " " < etc/isu$$n/services)"; done'
restart-test: ; bash tools/restart-test.sh
