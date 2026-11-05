// Package llm is the Go port of the part of Swift's Sources/App/LLM/Routing
// that group 4F and wave 5 need: the routed transport, its adapters
// (Anthropic, OpenAI-compatible, Hermes gateway), routers, failover
// classification, streaming and the meters the transport writes.
//
// Callers hand in an OpenAI chat-completions payload as bytes. Every adapter
// answers in the same OpenAI shape, so the rest of the server sees one wire
// format.
//
// Swift's task-local LLMRoutingContext is a context.Context value here, see
// WithValues. Streams are iter.Seq2 values: breaking out of the range loop
// cancels the upstream call.
//
// Not ported, and what that means, is listed in the PR that added the package:
// CerberusModelRouter and its profile, pool and free-lane machinery, the Gemini
// and Ollama adapters, and ParallelExecutor. Transport has the hooks they plug
// into (Decision.Cerberus, RouterTelemetry, EnsembleExecutor).
package llm
