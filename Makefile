.DEFAULT_GOAL := help

.PHONY: help dev test test-unit test-browser test-fixtures lint build clean-test-data

help:
	@printf '%s\n' \
		'dev              Run the current Go source with debug logging' \
		'test             Run unit and browser tests' \
		'test-unit        Run Go unit tests' \
		'test-browser     Set up fixtures and run Playwright tests' \
		'lint             Check Go formatting, run go vet, and check app JavaScript syntax' \
		'build            Build bin/fast-fotos' \
		'clean-test-data  Remove local browser-test data and photos'

dev:
	LOG_LEVEL=debug PHOTO_ROOT=./test-photos DATABASE_PATH=./test-data/fast-fotos.db go run ./cmd/fast-fotos

test: test-unit test-browser

test-unit:
	go test ./...

test-browser: test-fixtures
	npm run test:e2e

test-fixtures: clean-test-data
	mkdir -p test-data test-photos
	cp -a scripts/test-fixtures/test-data/. test-data/
	cp -a scripts/test-fixtures/test-photos/. test-photos/
	chmod 0777 test-data test-data/*

lint:
	@unformatted="$$(find cmd internal -type f -name '*.go' -print0 | xargs -0 gofmt -l)"; \
	if [ -n "$$unformatted" ]; then \
		printf 'Go files need formatting:\n%s\n' "$$unformatted"; \
		exit 1; \
	fi
	go vet ./...
	node -c internal/app/static/app.js

build:
	mkdir -p bin
	CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o bin/fast-fotos ./cmd/fast-fotos

clean-test-data:
	rm -rf test-data test-photos
	mkdir -p test-data test-photos
	chmod 0777 test-data test-photos
