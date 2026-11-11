# Stub upstream (see main.go). Built from the repo root: docker-compose.parity.yml
# `llmstub` service, and the stub-image CI job, which pushes it from main for
# hermes-stub on staging. Standard library only, so it copies just its own package.
FROM golang:1.27.4 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY parity/stubs/llm ./parity/stubs/llm
RUN CGO_ENABLED=0 go build -trimpath -o /out/llm ./parity/stubs/llm

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/llm /llm
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/llm"]
