.PHONY: build test run web clean

# build compiles the frontend first and stages it into internal/webui/dist
# so the binary embeds the web UI (see internal/webui/webui.go).
build: web
	mkdir -p internal/webui/dist
	cp -r web/dist/. internal/webui/dist/
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
	find internal/webui/dist -mindepth 1 -not -name .gitkeep -delete 2>/dev/null || true
