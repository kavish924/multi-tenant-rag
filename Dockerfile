# One Dockerfile for every binary:  docker build --build-arg BIN=api -t api .
# BIN is one of: api, worker, outbox-relay, migrate.

ARG GO_VERSION=1.27

FROM golang:${GO_VERSION} AS build
ARG BIN
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN test -n "$BIN" && test -d "cmd/$BIN" \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/app "./cmd/$BIN"

# Distroless static: no shell, no package manager, runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
