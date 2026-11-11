package main

import (
	"lumina/internal/hermescontainers"
	"lumina/internal/modules/plugins"
)

// pluginsModule is /v1/plugins (6F). Swift mounts it only with a secret box, and
// the hub install routes only beside a container manager, which exists whenever
// the box does. This process has no docker, so hub install answers as Swift does
// without a docker binary (owner decision O1).
func (a *agent) pluginsModule() *plugins.Module {
	if a.box == nil {
		return nil
	}
	http := plugins.DefaultHTTP{}
	return plugins.New(a.pool, plugins.Options{
		Identify: a.identify, Limits: a.limits, BYO: byoHermes(a.pool, a.cfg, a.box), Box: a.box,
		Importer: a.imports,
		Connectors: plugins.NewRegistry(
			plugins.ReadwiseConnector{HTTP: http}, plugins.RaindropConnector{HTTP: http},
			plugins.RSSConnector{HTTP: http}, plugins.NewsTickerConnector{},
		),
		Catalog:    a.skillCatalog(),
		Containers: hermescontainers.New(a.pool, a.box),
	})
}
