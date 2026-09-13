.PHONY: build test run web clean

build:
	go build -o bin/local-codex ./cmd/local-codex

test:
	go test ./...

vet:
	go vet ./...

run: build
	./bin/local-codex $(WORKSPACE)

web:
	cd web && npm install && npm run build

clean:
	rm -rf bin web/dist web/node_modules
