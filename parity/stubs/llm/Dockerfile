# Parity-only stub LLM (see main.go). Built from the repo root:
# docker-compose.parity.yml `llmstub` service.
FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY parity/stubs/llm ./parity/stubs/llm
RUN CGO_ENABLED=0 go build -trimpath -o /out/llm ./parity/stubs/llm

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/llm /llm
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/llm"]
