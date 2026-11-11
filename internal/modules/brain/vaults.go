package brain

import (
	"context"
	"errors"
	"lumina/internal/vaultaccess"
	"lumina/internal/wire"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// scope is a vault the caller has been confirmed to read.
type scope struct {
	ID       uuid.UUID
	Name     string
	Personal bool
}

// scopes returns the vaults the graph may read, sorted by name (case-insensitive) then id. Every
// id, whether it came from the candidate query or from the caller, goes through
// vaultaccess.ResolveVault with Read: that is the only thing that admits a vault.
//
// With no requested ids the candidates are filtered: a vault the caller turns out
// not to read (a *wire.Error such as an archived team) is dropped silently. With
// requested ids the first refusal is returned as is, so the caller learns that
// the ask was not granted instead of getting a quietly smaller graph.
func (m *Module) scopes(ctx context.Context, userID uuid.UUID, requested []uuid.UUID) ([]scope, error) {
	type candidate struct {
		id   uuid.UUID
		name string
	}
	var cands []candidate
	filter := len(requested) == 0
	if filter {
		rows, err := m.q.CandidateVaults(ctx, userID)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			cands = append(cands, candidate{r.ID, r.Name})
		}
	} else {
		seen := map[uuid.UUID]bool{}
		for _, id := range requested {
			if !seen[id] {
				seen[id] = true
				cands = append(cands, candidate{id: id})
			}
		}
	}

	out := make([]scope, 0, len(cands))
	for _, cand := range cands {
		acc, err := m.access.ResolveVault(ctx, userID, cand.id, vaultaccess.Read)
		if err != nil {
			var we *wire.Error
			if filter && errors.As(err, &we) {
				continue
			}
			return nil, err
		}
		name := cand.name
		if !filter {
			if name, err = m.q.VaultName(ctx, acc.VaultID); err != nil {
				return nil, err
			}
		}
		out = append(out, scope{ID: acc.VaultID, Name: name, Personal: acc.IsPersonal})
	}
	sort.Slice(out, func(i, j int) bool {
		// Case-insensitive first, so "alice's Vault" sorts before "Team Vault".
		if a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name); a != b {
			return a < b
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return strings.Compare(out[i].ID.String(), out[j].ID.String()) < 0
	})
	return out, nil
}
