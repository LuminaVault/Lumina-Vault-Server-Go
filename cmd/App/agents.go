package main

import (
	"lumina/internal/hermesresolve"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/modules/agents"
	"lumina/internal/ssrfguard"
	"net/url"
	"strings"
)

// agentsModule builds /v1/agents (wave 5D). Rooms speak through the stack's routed
// chat service, Swift's llmService, and reach a BYO gateway through the resolver,
// which is nil without a SecretBox or a gateway URL that parses, as in Swift.
func (a *agent) agentsModule() httpserver.Module {
	opts := agents.Options{Identify: a.identify, Limits: a.limits, Chat: a.stack.ChatService(a.cfg.HermesDefaultModel), HTTP: llm.NoRedirectClient()}
	if a.box != nil {
		if gateway, err := url.Parse(a.cfg.HermesGatewayURL); err == nil {
			opts.Resolver = hermesresolve.New(a.pool, a.box, ssrfguard.Guard{
				AllowPrivateRanges: strings.EqualFold(a.cfg.BYOHermesAllowPrivate, "true"),
				RequireHTTPS:       a.cfg.BYOHermesRequireHTTPS(),
				AllowTailnetHTTP:   strings.EqualFold(a.cfg.BYOHermesAllowTailnet, "true"),
			}, gateway)
		}
	}
	return agents.New(a.pool, opts)
}
