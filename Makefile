.PHONY: build build-go run clean test web web-install vet
BINARY=notion-manager
build: web build-go
build-go:
	go build -o $(BINARY) ./cmd/notion-manager/
run: build
	./$(BINARY)
web-install:
	cd web && npm ci
web:
	cd web && npm run build
	python3 -c "import shutil; shutil.rmtree('internal/web/dist', ignore_errors=True); shutil.copytree('web/dist', 'internal/web/dist')"
clean:
	python3 -c "import pathlib, shutil; pathlib.Path('$(BINARY)').unlink(missing_ok=True); shutil.rmtree('web/dist', ignore_errors=True)"
test:
	go test ./...
vet:
	go vet ./...
