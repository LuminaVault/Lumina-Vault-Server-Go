// Package llm is the Go port of the part of Swift's Sources/App/LLM/Routing
// that group 4F and wave 5 need: the routed transport, its adapters (Anthropic,
// OpenAI-compatible, Hermes gateway, Gemini, Ollama), routers including Cerberus and
// the free lane, failover classification, streaming and the meters the transport
// writes.
//
// Callers hand in an OpenAI chat-completions payload as bytes. Every adapter
// answers in the same OpenAI shape, so the rest of the server sees one wire
// format.
//
// Swift's task-local LLMRoutingContext is a context.Context value here, see
// WithValues. Streams are iter.Seq2 values: breaking out of the range loop
// cancels the upstream call.
//
// Request-path callers use two services, built once from the Stack:
// Stack.StreamService (RoutedHermesLLMStreamService, chat and /v1/query/stream) and
// Stack.ChatService (RoutedHermesLLMService, /v1/llm/chat). Both resolve model
// disclosure through DisclosureFor; ClassifyToolErrors and SanitizeToolOutput are
// HermesToolErrorClassifier; IsEmptyCompletion and EmptyCompletionMessage are
// ChatStreamCompletionPolicy.
//
// Not ported is ParallelExecutor (cerberus.parallelEnabled is off in production).
// Transport has the hook it plugs into (EnsembleExecutor). The read side of
// credentials and preferences is here, their write side moves with wave 5.
package llm
