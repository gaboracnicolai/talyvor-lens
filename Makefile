.PHONY: build test vet status bench bench-compare

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
