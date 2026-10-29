# Same runtime contract as the Swift image (LuminaVaultServer/Dockerfile) so
# platform/infra charts/app runs either one: /app/App, ENTRYPOINT ./App,
# --http-host/--http-port, port 8080, and UID/GID 999 — the `hummingbird`
# system user that owns the files already on the api-*-data PVCs
# (decisions.md D3).

FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/App ./cmd/App \
    && mkdir -p /out/data/luminavault

FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build --chown=999:999 /out/App /app/App
# Pre-create the tenant vault root so a fresh named volume mounted at
# /app/data seeds its ownership from here (same reason as the Swift image).
COPY --from=build --chown=999:999 /out/data /app/data
USER 999:999
EXPOSE 8080
ENTRYPOINT ["./App"]
CMD ["--http-host", "0.0.0.0", "--http-port", "8080"]
