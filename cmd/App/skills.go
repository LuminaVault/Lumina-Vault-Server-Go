package main

import (
	"lumina/internal/apns"
	"lumina/internal/devicerpc"
	"lumina/internal/devicetools"
	"lumina/internal/entitlement"
	"lumina/internal/modules/calendar"
	"lumina/internal/secretbox"
	"lumina/internal/skills"
	"lumina/internal/usagemeter"
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

// skillsModule is /v1/skills (list, patch, runs, run, slash, outputs) and
// /v1/jobs. The runner's device tools reach the phone through this process's
// broker, so skills flips with /v1/ws (flip-blockers.md, the ws bundle).
// Run after modules(), which builds the kb-compile route /kb-compile uses.
func (a *agent) skillsModule(broker *devicerpc.Broker) *skillsroute.Module {
	cfg := a.cfg
	push := apns.New(a.pool, apns.Config{
		Enabled: cfg.APNSEnabled, BundleID: cfg.APNSBundleID, TeamID: cfg.APNSTeamID, KeyID: cfg.APNSKeyID,
		PrivateKeyPath: cfg.APNSPrivateKeyPath, Environment: cfg.APNSEnvironment,
	})
	var google *calendar.Google
	if box, err := secretbox.New(cfg.LVSecretMasterKey); err == nil {
		google = calendar.NewGoogle(a.pool, calendar.GoogleOptions{
			Box: box, ClientID: cfg.GoogleCalendarClientID, ClientSecret: cfg.GoogleCalendarClientSecret,
			RedirectURI: cfg.GoogleCalendarRedirectURI, WebReturnOrigins: cfg.CORSAllowedOrigins,
		})
	}
	runnerOpts := skillsroute.RunnerOptions{
		Transport: a.stack.Transport, DefaultModel: cfg.HermesDefaultModel, Paths: a.paths,
		Memories: a.memories, Embeddings: a.embeddings, APNS: push, Meter: usagemeter.New(a.pool),
		Proactive: apns.NewProactiveDelivery(a.pool, push),
		Device:    devicetools.New(a.pool, broker, devicerpc.NewQueue(a.pool, push)),
		Google:    google,
	}
	if a.today != nil { // a nil *me.Module must not become a non-nil interface
		runnerOpts.Brief = a.today
	}
	runner := skillsroute.NewRunner(a.pool, runnerOpts)
	opts := skillsroute.Options{
		Identify: a.identify, Limits: a.limits, Catalog: a.skillCatalog(), Paths: a.paths,
		// Swift builds HermesMirrorService only with a secret box and an endpoint resolver.
		Mirror:    a.box != nil && hermesResolver(a.pool, a.cfg, a.box) != nil,
		Transport: a.stack.Transport, DefaultModel: a.cfg.HermesDefaultModel,
		EnforcementEnabled: entitlement.EnforcementEnabled(a.cfg.BillingEnforcementEnabled),
		HasCredential:      entitlement.PGCredentials(a.pool, a.box),
		Runner:             runner,
	}
	if a.compile != nil {
		opts.Compile = a.compile.Compile
	}
	return skillsroute.New(a.pool, opts)
}
