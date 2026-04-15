BINARY := eiroute
PKG := ./cmd/eiroute
APP_VERSION ?= dev
LD_FLAGS := -X main.version=$(APP_VERSION)

.PHONY: build test lint docker run clean

build:
	go build -ldflags "$(LD_FLAGS)" -o $(BINARY) $(PKG)

test:
	go test -race ./...

lint:
	go vet ./...
	golangci-lint run

docker:
	docker build -t $(BINARY) --build-arg APP_VERSION=$(APP_VERSION) .

run: build
	./$(BINARY) -config config.example.yaml

clean:
	rm -f $(BINARY)
