# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/rclient-server ./cmd/server

# Tiny "scratch + ca-certs + state dir" stage. distroless `nonroot` runs as
# uid 65532; we pre-create /var/lib/rclient with the right owner so the
# enrollment registry is writable when the volume is bind-mounted on top.
FROM busybox:1-musl AS prep
RUN mkdir -p /state && chown 65532:65532 /state && chmod 0700 /state

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/rclient-server /rclient-server
COPY --from=prep --chown=65532:65532 /state /var/lib/rclient
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/rclient-server"]
