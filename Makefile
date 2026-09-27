.PHONY: build test race vet release
build:
	mkdir -p dist
	go build -buildvcs=false -o dist/rcm ./cmd/rcm
test:
	go test ./... -count=1
race:
	go test -race ./... -count=1
vet:
	go vet ./...
release:
	./scripts/release.sh
