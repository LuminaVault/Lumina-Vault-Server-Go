package spaces

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// defaultSpaces are seeded on first POST /v1/vault/create (Vault, wave 4).
// Slugs are part of the public contract (SpaceDefaults.swift).
var defaultSpaces = []struct{ slug, name, icon string }{
	{"ai", "AI", "sparkles"},
	{"stocks", "Stocks", "chart.line.uptrend.xyaxis"},
	{"health", "Health", "heart.fill"},
	{"work", "Work", "briefcase.fill"},
	{"ideas", "Ideas", "lightbulb.fill"},
}

// SeedDefaults is SpacesService.seedDefaults: it creates every default space
// the vault does not already own (category = slug) and returns the slugs it
// created. Swift's service create leaves created_by/updated_by unset here.
func (m *Module) SeedDefaults(ctx context.Context, vault uuid.UUID) ([]string, error) {
	var created []string
	for _, d := range defaultSpaces {
		icon, category := d.icon, d.slug
		_, err := m.createSpace(ctx, vault, uuid.Nil, d.name, d.slug, nil, nil, &icon, &category)
		if errors.Is(err, errSlugTaken) {
			continue
		}
		if err != nil {
			return created, err
		}
		created = append(created, d.slug)
	}
	return created, nil
}
