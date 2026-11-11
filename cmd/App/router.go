package main

import (
	"lumina/internal/httpserver"
	"lumina/internal/modules/llmadmin"
	"lumina/internal/modules/llmrouter"
	"lumina/internal/modules/meproviders"
)

// routerModules builds wave 5A's modules on the agent's one LLM stack, so a
// credential or preference write drops the cache the router reads.
func (a *agent) routerModules() []httpserver.Module {
	return []httpserver.Module{
		meproviders.New(a.pool, meproviders.Options{Identify: a.identify, Limits: a.limits, Stack: a.stack, Box: a.box}),
		llmrouter.New(a.pool, llmrouter.Options{
			Identify: a.identify, Limits: a.limits, Stack: a.stack, Memories: a.memories, Embeddings: a.embeddings,
		}),
		llmadmin.New(a.stack.Registry, a.cfg.AdminToken, nil),
	}
}
