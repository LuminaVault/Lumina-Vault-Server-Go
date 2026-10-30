// Package vaultpaths lays out tenant vault directories exactly as
// LuminaVaultServer Sources/App/Services/VaultPathService.swift does:
// <root>/tenants/<UPPERCASE UUID>/{raw,wiki,memories}. Both servers share the
// PVC, so a path that differs in case or shape is a different folder.
package vaultpaths

import (
	"lumina/internal/ids"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// dirPerm is what Foundation's createDirectory uses with no attributes.
const dirPerm = 0o755

type Paths struct{ Root string }

func (p Paths) TenantRoot(tenant uuid.UUID) string {
	return filepath.Join(p.Root, "tenants", ids.Upper(tenant))
}

func (p Paths) Raw(tenant uuid.UUID) string { return filepath.Join(p.TenantRoot(tenant), "raw") }

func (p Paths) Wiki(tenant uuid.UUID) string { return filepath.Join(p.TenantRoot(tenant), "wiki") }

func (p Paths) Memories(tenant uuid.UUID) string {
	return filepath.Join(p.TenantRoot(tenant), "memories")
}

// EnsureTenantDirectories creates raw, wiki and memories (and parents).
func (p Paths) EnsureTenantDirectories(tenant uuid.UUID) error {
	for _, d := range []string{p.Raw(tenant), p.Wiki(tenant), p.Memories(tenant)} {
		if err := os.MkdirAll(d, dirPerm); err != nil {
			return err
		}
	}
	return nil
}
