.PHONY: test benchmark run fmt

test:
	go test ./...

benchmark:
	go run ./cmd/benchmark -requests 10000 -workers 64

run:
	go run ./cmd/server

fmt:
	gofmt -w ./cmd ./internal
