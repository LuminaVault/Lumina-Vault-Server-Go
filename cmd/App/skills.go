package main

import (
	"lumina/internal/entitlement"
	"lumina/internal/httpserver"
	"lumina/internal/skills"
	"strings"

	skillsroute "lumina/internal/modules/skills"
)

// skillCatalog is SkillCatalog: builtin plus vault skills, or vault only when
// skills.builtinScan.enabled is false.
func (a *agent) skillCatalog() *skills.Catalog {
	if strings.EqualFold(a.cfg.SkillsBuiltinScanEnabled, "false") {
		return skills.NewVaultCatalog(a.paths)
	}
	return skills.NewCatalog(a.paths)
}

// skillsModule is /v1/skills (list, patch, runs, outputs) and /v1/jobs.
func (a *agent) skillsModule() httpserver.Module {
	return skillsroute.New(a.pool, skillsroute.Options{
		Identify: a.identify, Limits: a.limits, Catalog: a.skillCatalog(), Paths: a.paths,
		// Swift builds HermesMirrorService only with a secret box and an endpoint resolver.
		Mirror:    a.box != nil && hermesResolver(a.pool, a.cfg, a.box) != nil,
		Transport: a.stack.Transport, DefaultModel: a.cfg.HermesDefaultModel,
		EnforcementEnabled: entitlement.EnforcementEnabled(a.cfg.BillingEnforcementEnabled),
		HasCredential:      entitlement.PGCredentials(a.pool, a.box),
	})
}
