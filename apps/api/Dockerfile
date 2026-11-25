# Multi-stage build for the Console API. The context is the repository root so
# the module cache and source stay in one layer set.
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY apps/api/go.mod apps/api/go.sum ./apps/api/
RUN cd apps/api && go mod download
COPY apps/api ./apps/api
RUN cd apps/api && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/roost-console ./cmd/roost-console

# BusyBox keeps the runtime layer small and provides wget for the container
# healthcheck without apt/apk network access at build time.
FROM busybox:1.36
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/roost-console /usr/local/bin/roost-console
USER 65534:65534
EXPOSE 8792
ENTRYPOINT ["/usr/local/bin/roost-console"]
