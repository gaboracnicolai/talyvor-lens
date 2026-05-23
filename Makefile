.PHONY: build test vet status

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
