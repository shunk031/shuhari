.PHONY: setup
setup:
	mise install
	mise exec -- prek install

.PHONY: test
test:
	go test -race ./...

.PHONY: lint
lint:
	go vet ./...
	mise exec -- prek run --all-files
