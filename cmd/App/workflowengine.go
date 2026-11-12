package main

import (
	"log/slog"
	"lumina/internal/apns"
	"lumina/internal/jobs"
	"lumina/internal/memorystore"
	skillsroute "lumina/internal/modules/skills"
	"lumina/internal/modules/workflows"
)

// workflowEngineJob is WorkflowEngine as App+build.swift builds it: the routed
// transport, HERMES_DEFAULT_MODEL, the skill runner and catalog (behind the skills
// module), embeddings, router profiles, the workflow spend service and APNs.
func (a *agent) workflowEngineJob(wf *workflows.Module, skills *skillsroute.Module, memories *memorystore.Repository) jobs.Job {
	cfg := a.cfg
	return wf.NewEngine(workflows.EngineDeps{
		Transport:    a.stack.Transport,
		DefaultModel: cfg.HermesDefaultModel,
		Skills:       skills,
		Embeddings:   a.embeddings,
		Memories:     memories,
		Profiles:     a.stack.Profiles,
		Push: apns.New(a.pool, apns.Config{
			Enabled: cfg.APNSEnabled, BundleID: cfg.APNSBundleID, TeamID: cfg.APNSTeamID, KeyID: cfg.APNSKeyID,
			PrivateKeyPath: cfg.APNSPrivateKeyPath, Environment: cfg.APNSEnvironment,
		}),
		Workers: cfg.CerberusStudioWorkerCount,
		Log:     slog.Default().With("label", "lv.workflows.worker"),
	}).Job()
}
