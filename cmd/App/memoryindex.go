package main

import (
	"context"
	"log/slog"
	"lumina/internal/jobs"
	"lumina/internal/keepjudge"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/workers"
	"strings"

	"github.com/google/uuid"
)

// memoryIndexJob is MemoryIndexWorker, registered as App+build.swift does: Fluent on
// and not the test environment. MEMORY_KEEP_GATE_ENABLED=true adds the keep judge on
// the routed transport, with BYOK tenants never judged.
func (a *agent) memoryIndexJob(memories *memorystore.Repository) []jobs.Job {
	if a.cfg.LVEnvironment == "test" {
		return nil
	}
	w := &workers.MemoryIndex{
		Pool: a.pool, Embeddings: a.embeddings, Memories: memories,
		Log: slog.Default().With("label", "lv.memory.index.worker"),
	}
	if strings.EqualFold(a.cfg.MemoryKeepGateEnabled, "true") {
		prefs := a.stack.Preferences
		w.Judge = keepjudge.Judge{
			Transport: a.stack.Transport,
			Model:     a.cfg.HermesDefaultModel,
			IsBYOK: func(ctx context.Context, tenant uuid.UUID) bool {
				p, err := prefs.Get(ctx, tenant)
				return err == nil && p != nil && p.Mode == llm.ModeBYOK
			},
		}
	}
	return []jobs.Job{w.Job()}
}
