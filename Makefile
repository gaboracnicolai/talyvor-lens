.PHONY: build test vet status bench bench-compare up down logs ps reset

build:
	go build ./...

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
