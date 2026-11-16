package main

import (
	"lumina/internal/config"
	"lumina/internal/ssrfguard"
	"strings"
)

// byokGuard checks the base URLs tenants store with their own provider keys
// (/v1/me/providers). It uses the BYO Hermes switches for private and tailnet
// ranges, but not RequireHTTPS: that one is about Hermes credentials in transit,
// and a tenant's plain-http Ollama on their tailnet is a supported setup.
func byokGuard(cfg config.Config) ssrfguard.Guard {
	return ssrfguard.Guard{
		AllowPrivateRanges: strings.EqualFold(cfg.BYOHermesAllowPrivate, "true"),
		AllowTailnetHTTP:   strings.EqualFold(cfg.BYOHermesAllowTailnet, "true"),
	}
}

// userFetchGuard is for pages and feeds a user hands us (captured links, RSS,
// Readwise and Raindrop). Nothing private, nothing on the tailnet.
var userFetchGuard = ssrfguard.Guard{}
