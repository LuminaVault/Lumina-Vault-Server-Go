package hermesmirror

import (
	"context"
	"lumina/internal/hermesdash"
	"sync"

	"github.com/google/uuid"
)

// requireDashboard: only a remote Hermes with a linked dashboard has a
// checkout to inspect.
func (s *Service) requireDashboard(ctx context.Context, tenant uuid.UUID) (*hermesdash.Dashboard, error) {
	transport, err := s.Transports.Transport(ctx, tenant)
	if err != nil {
		return nil, err
	}
	remote, ok := transport.(*hermesdash.Remote)
	if !ok || remote.Dashboard == nil {
		return nil, hermesdash.Unsupported("workspace")
	}
	return remote.Dashboard, nil
}

// WorkspaceStatus is workspaceStatus: four upstream reads run concurrently;
// the first failure in declaration order wins, as with Swift's async lets.
func (s *Service) WorkspaceStatus(ctx context.Context, tenant uuid.UUID, path string) (WorkspaceStatusDTO, error) {
	d, err := s.requireDashboard(ctx, tenant)
	if err != nil {
		return WorkspaceStatusDTO{}, err
	}
	var (
		wg        sync.WaitGroup
		repo      hermesdash.Repo
		branches  []hermesdash.Branch
		worktrees []hermesdash.Worktree
		changes   []hermesdash.ChangedFile
		errs      [4]error
	)
	wg.Add(4)
	go func() { defer wg.Done(); repo, errs[0] = d.GitStatus(ctx, path) }()
	go func() { defer wg.Done(); branches, errs[1] = d.GitBranches(ctx, path) }()
	go func() { defer wg.Done(); worktrees, errs[2] = d.GitWorktrees(ctx, path) }()
	go func() { defer wg.Done(); changes, errs[3] = d.GitChangedFiles(ctx, path) }()
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return WorkspaceStatusDTO{}, err
		}
	}
	out := WorkspaceStatusDTO{
		Repo:      WorkspaceRepoDTO{Root: repo.Root, Branch: repo.Branch, IsDetached: repo.IsDetached},
		Branches:  make([]WorkspaceBranchDTO, 0, len(branches)),
		Worktrees: make([]WorkspaceWorktreeDTO, 0, len(worktrees)),
		Changes:   make([]WorkspaceChangeDTO, 0, len(changes)),
	}
	for _, b := range branches {
		out.Branches = append(out.Branches, WorkspaceBranchDTO{Name: b.Name, IsCurrent: b.IsCurrent})
	}
	for _, w := range worktrees {
		out.Worktrees = append(out.Worktrees, WorkspaceWorktreeDTO{Path: w.Path, Branch: w.Branch, IsPrimary: w.IsPrimary})
	}
	for _, c := range changes {
		out.Changes = append(out.Changes, WorkspaceChangeDTO{Path: c.Path, Added: c.Added, Removed: c.Removed, Status: c.Status})
	}
	return out, nil
}

// WorkspaceDiff is workspaceDiff.
func (s *Service) WorkspaceDiff(ctx context.Context, tenant uuid.UUID, repoPath, file string) (WorkspaceDiffDTO, error) {
	d, err := s.requireDashboard(ctx, tenant)
	if err != nil {
		return WorkspaceDiffDTO{}, err
	}
	diff, err := d.GitFileDiff(ctx, repoPath, file)
	if err != nil {
		return WorkspaceDiffDTO{}, err
	}
	return WorkspaceDiffDTO{Path: diff.Path, Diff: diff.Diff, Truncated: diff.Truncated}, nil
}

// WorkspaceListing is workspaceListing.
func (s *Service) WorkspaceListing(ctx context.Context, tenant uuid.UUID, path string) (WorkspaceListingDTO, error) {
	d, err := s.requireDashboard(ctx, tenant)
	if err != nil {
		return WorkspaceListingDTO{}, err
	}
	entries, err := d.ListFiles(ctx, path)
	if err != nil {
		return WorkspaceListingDTO{}, err
	}
	out := WorkspaceListingDTO{Path: path, Entries: make([]WorkspaceFileEntryDTO, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, WorkspaceFileEntryDTO{Name: e.Name, Path: e.Path, IsDirectory: e.IsDirectory})
	}
	return out, nil
}

// WorkspaceFile is workspaceFile.
func (s *Service) WorkspaceFile(ctx context.Context, tenant uuid.UUID, path string) (WorkspaceFileDTO, error) {
	d, err := s.requireDashboard(ctx, tenant)
	if err != nil {
		return WorkspaceFileDTO{}, err
	}
	content, err := d.ReadText(ctx, path)
	if err != nil {
		return WorkspaceFileDTO{}, err
	}
	return WorkspaceFileDTO{Path: path, Content: content}, nil
}
