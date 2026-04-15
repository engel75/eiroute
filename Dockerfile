FROM golang:1.26.2-alpine AS builder
ARG APP_VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${APP_VERSION}" -o /app/eiroute ./cmd/eiroute

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl iputils-ping iproute2 \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 1000 nonroot \
    && useradd --uid 1000 --gid nonroot --shell /bin/sh nonroot
COPY --from=builder /app/eiroute /app/eiroute
WORKDIR /app
USER nonroot:nonroot
ENTRYPOINT ["/app/eiroute"]
