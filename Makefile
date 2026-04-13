BINARY := eiroute
PKG := ./cmd/eiroute

.PHONY: build test lint docker run clean

build:
	go build -o $(BINARY) $(PKG)

test:
	go test -race ./...

lint:
	go vet ./...
	golangci-lint run

docker:
	docker build -t $(BINARY) .

run: build
	./$(BINARY) -config config.example.yaml

clean:
	rm -f $(BINARY)
