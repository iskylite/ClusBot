.PHONY: build test release
RELEASE_ARCHIVE = dist/clusbot-$(shell go env GOOS)-$(shell go env GOARCH).tar.gz

build:
	npm --prefix web ci
	npm --prefix web run build
	mkdir -p bin
	go build -trimpath -o bin/clusbot ./cmd/clusbot

test:
	npm --prefix web run build
	go test ./...

release: build
	mkdir -p dist
	COPYFILE_DISABLE=1 tar -czf "$(RELEASE_ARCHIVE)" bin/clusbot configs/example.yaml README.md
