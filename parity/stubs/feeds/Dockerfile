# Parity-only stub feed aggregator (see main.go). Built from the repo root:
# docker-compose.parity.yml `feeds` service.
FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY parity/stubs/feeds ./parity/stubs/feeds
RUN CGO_ENABLED=0 go build -trimpath -o /out/feeds ./parity/stubs/feeds

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/feeds /feeds
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/feeds"]
