.PHONY: build test vet status bench bench-compare up down logs ps reset \
        binaries lens node cachenode embednode \
        shellcheck backup-verify-local

build:
	go build ./...

# ────────────────────────────────────────────────────────────
# Backup & Disaster Recovery (Upgrade 12). The scripts + runbook live in
# deploy/backup/. These targets statically check the scripts and run a LOCAL
# backup→restore→verify round-trip in a throwaway docker Postgres — the only
# DR proof obtainable without real infrastructure. A real production restore
# drill (backup_verify.sh against prod backups) is a manual operational task.
# ────────────────────────────────────────────────────────────

# Lint every backup script. Requires shellcheck (brew install shellcheck).
shellcheck:
	shellcheck deploy/backup/scripts/*.sh

# Prove the backup machinery round-trips locally (needs docker).
backup-verify-local:
	deploy/backup/scripts/local_drill.sh

# ────────────────────────────────────────────────────────────
# Mining binaries — one go-build per cmd/ subdirectory. The
# Lens server itself is the same binary as `make build` (the
# default builds every cmd/), but we keep a named target so the
# release process can `make lens node cachenode embednode` in
# parallel.
# ────────────────────────────────────────────────────────────

# Build all four binaries to ./bin/.
binaries: lens node cachenode embednode

lens:
	go build -o bin/talyvor-lens ./cmd/lens

node:
	go build -o bin/talyvor-node ./cmd/node

cachenode:
	go build -o bin/talyvor-cachenode ./cmd/cachenode

embednode:
	go build -o bin/talyvor-embednode ./cmd/embednode

test:
	go test ./...

vet:
	go vet ./...

# Pretty-print the live status page JSON. Assumes Lens is running on
# localhost:8080. Customers + on-call use this from CI shells.
status:
	curl -s http://localhost:8080/status.json | python3 -m json.tool

# Run the performance benchmark suite. Gated behind the `bench` build
# tag so a regular `go test ./...` skips it — the suite takes minutes.
bench:
	cd benchmarks && go test -tags=bench -bench=. -benchmem \
	  -benchtime=10s ./... | tee benchmark-results.txt

# Multi-run benchmark for variance analysis. count=5 gives benchstat
# enough samples to compute meaningful confidence intervals.
bench-compare:
	cd benchmarks && go test -tags=bench -bench=. -benchmem \
	  -benchtime=10s -count=5 ./...

# ────────────────────────────────────────────────────────────
# Docker compose lifecycle. Run `cp .env.production.example .env`
# first; `make up` starts everything (Lens + Postgres + Redis + NATS).
# ────────────────────────────────────────────────────────────

# Bring the full stack up in the background.
up:
	docker compose up -d

# Stop services; keep volumes (data survives).
down:
	docker compose down

# Tail the Lens container logs.
logs:
	docker compose logs -f lens

# Show the current state of every service in the compose project.
ps:
	docker compose ps

# Nuke everything: stop services AND delete data volumes. Use when
# you want a clean DB / cache state — destructive.
reset:
	docker compose down -v && docker compose up -d
