CONFIG ?= configs/local.yaml
ENV_FILE ?= $(if $(wildcard .runtime/dev.env),.runtime/dev.env,.runtime/env)
RUN = python3 deploy/with-env.py $(ENV_FILE)
.PHONY: test check api media agent coordinator migrate seed storage-init web dev runtime-check local-init local-up local-down
test:
	go test ./...
	cd web && npm test
	python3 -m unittest discover -s deploy -p 'test_*.py'
	python3 -m unittest discover -s eval
check:
	go vet ./...
	cd web && npm run typecheck && npm run build && npm run format:check
api:
	$(RUN) go run ./cmd/api -config $(CONFIG)
media:
	$(RUN) go run ./cmd/worker -config $(CONFIG) -kind media
agent:
	$(RUN) go run ./cmd/worker -config $(CONFIG) -kind agent
coordinator:
	$(RUN) go run ./cmd/coordinator -config $(CONFIG)
migrate:
	$(RUN) go run ./cmd/devtool -config $(CONFIG) -action migrate
seed:
	$(RUN) go run ./cmd/devtool -config $(CONFIG) -action seed
storage-init:
	$(RUN) go run ./cmd/devtool -config $(CONFIG) -action init-storage
web:
	cd web && npm run dev -- --host 127.0.0.1

dev:
	$(RUN) python3 deploy/dev.py --config $(CONFIG)
runtime-check:
	$(RUN) go run ./cmd/devtool -config $(CONFIG) -action check

local-init:
	python3 deploy/local.py init
local-up:
	python3 deploy/local.py up
local-down:
	python3 deploy/local.py down
