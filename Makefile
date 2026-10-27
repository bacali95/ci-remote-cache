.PHONY: build test run docker web

web:
	cd web && bun install --frozen-lockfile && bun run build

build: web
	go build -o bin/server ./cmd/server

test:
	go test ./... -race -cover -p 1

run: build
	./bin/server

docker:
	docker build -t ci-remote-cache:local .
