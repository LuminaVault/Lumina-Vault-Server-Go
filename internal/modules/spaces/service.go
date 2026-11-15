package spaces

import (
	"context"
	"fmt"
	"lumina/internal/modules/spaces/db"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// The methods below are what the /v1/spaces handlers do once the caller's
// access to the vault is settled. They take the vault and the acting user
// directly, so code that has no HTTP request (the assistant's tools) changes
// spaces through the same validation, folders and activity rows as the API.
// The caller owns the access check.

// CreateInput is the body of POST /v1/spaces. A nil or blank Slug is derived
// from Name.
type CreateInput struct {
	Name        string
	Slug        *string
	Description *string
	Color       *string
	Icon        *string
	Category    *string
}

// UpdateInput is the body of PUT /v1/spaces/:id: nil keeps a field, "" clears
// it. A slug never changes.
type UpdateInput struct {
	Name        *string
	Description *string
	Color       *string
	Icon        *string
	Category    *string
}

// List returns the vault's spaces by name, with live note counts.
func (m *Module) List(ctx context.Context, vault uuid.UUID) ([]SpaceDTO, error) {
	rows, err := m.q.ListSpaces(ctx, vault)
	if err != nil {
		return nil, err
	}
	counts, err := m.noteCounts(ctx, vault, rows)
	if err != nil {
		return nil, err
	}
	out := make([]SpaceDTO, 0, len(rows))
	for _, s := range rows {
		out = append(out, toDTO(s, counts[s.ID]))
	}
	return out, nil
}

// Create makes the space and its folder and records space.created.
func (m *Module) Create(ctx context.Context, vault, actor uuid.UUID, in CreateInput) (SpaceDTO, error) {
	s, err := m.createSpace(ctx, vault, actor, in.Name, resolveSlug(in.Slug, in.Name),
		in.Description, in.Color, in.Icon, in.Category)
	if err != nil {
		return SpaceDTO{}, err
	}
	if err := m.record(ctx, vault, actor, "space.created", s.ID, s.Name); err != nil {
		return SpaceDTO{}, err
	}
	return toDTO(s, 0), nil
}

// Update changes the fields in is non-nil for and records space.updated.
func (m *Module) Update(ctx context.Context, vault, actor, id uuid.UUID, in UpdateInput) (SpaceDTO, error) {
	s, n, err := m.load(ctx, vault, id)
	if err != nil {
		return SpaceDTO{}, err
	}
	if in.Name != nil {
		name := trim(*in.Name)
		if name == "" {
			return SpaceDTO{}, errNameRequired
		}
		s.Name = name
	}
	s.Description = setOrClear(s.Description, in.Description)
	s.Color = setOrClear(s.Color, in.Color)
	s.Icon = setOrClear(s.Icon, in.Icon)
	s.Category = setOrClear(s.Category, in.Category)
	now := m.now()
	s, err = m.q.UpdateSpace(ctx, db.UpdateSpaceParams{
		TenantID: vault, ID: id, Name: s.Name, Description: s.Description, Color: s.Color,
		Icon: s.Icon, Category: s.Category, NoteCount: int32(n), UpdatedByUserID: &actor, UpdatedAt: &now, //nolint:gosec // a row count
	})
	if err != nil {
		return SpaceDTO{}, err
	}
	if err := m.record(ctx, vault, actor, "space.updated", id, s.Name); err != nil {
		return SpaceDTO{}, err
	}
	return toDTO(s, n), nil
}

// Delete removes the space and records space.deleted. The folder is renamed
// to _deleted_<unix>_<slug>, so the notes in it stay on disk. It returns the
// deleted space's name.
func (m *Module) Delete(ctx context.Context, vault, actor, id uuid.UUID) (string, error) {
	s, _, err := m.load(ctx, vault, id)
	if err != nil {
		return "", err
	}
	// Soft-remove the folder so an accidental delete loses no notes.
	folder := filepath.Join(m.paths.Raw(vault), s.Slug)
	if _, err := os.Stat(folder); err == nil {
		stamped := filepath.Join(m.paths.Raw(vault), fmt.Sprintf("_deleted_%d_%s", m.now().Unix(), s.Slug))
		// FileManager.moveItem refuses an existing destination; rename(2) would replace an empty one.
		if _, err := os.Lstat(stamped); err == nil {
			return "", fmt.Errorf("spaces: %s already exists", stamped)
		}
		if err := os.Rename(folder, stamped); err != nil {
			return "", err
		}
	}
	if err := m.q.DeleteSpace(ctx, db.DeleteSpaceParams{TenantID: vault, ID: id}); err != nil {
		return "", err
	}
	if err := m.record(ctx, vault, actor, "space.deleted", id, s.Name); err != nil {
		return "", err
	}
	return s.Name, nil
}
