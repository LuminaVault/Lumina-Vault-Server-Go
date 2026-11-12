package hermesmirror

import (
	"lumina/internal/hermescaps"
	"lumina/internal/wire"
	"time"
)

// StatusDTO is HermesMirrorStatusDTO.
type StatusDTO struct {
	Transport        string                `json:"transport"`
	LastSyncAt       *wire.Time            `json:"lastSyncAt,omitempty"`
	LastStatus       string                `json:"lastStatus"`
	LastError        *string               `json:"lastError,omitempty"`
	SkillsCount      int64                 `json:"skillsCount"`
	JobsCount        int64                 `json:"jobsCount"`
	VaultFilesCount  int64                 `json:"vaultFilesCount"`
	VaultPath        *string               `json:"vaultPath,omitempty"`
	VaultState       string                `json:"vaultState"`
	SessionsImported int64                 `json:"sessionsImported"`
	CompileJobID     *string               `json:"compileJobID,omitempty"`
	Dashboard        *hermescaps.Dashboard `json:"dashboard,omitempty"`
}

// SkillDTO is HermesMirroredSkillDTO.
type SkillDTO struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Enabled     bool       `json:"enabled"`
	Source      string     `json:"source"`
	UpdatedAt   *wire.Time `json:"updatedAt,omitempty"`
}

// SkillsResponse is HermesMirroredSkillsResponse.
type SkillsResponse struct {
	Skills []SkillDTO `json:"skills"`
}

// JobDTO is HermesMirroredJobDTO.
type JobDTO struct {
	HermesJobID string     `json:"hermesJobID"`
	Name        *string    `json:"name,omitempty"`
	Schedule    *string    `json:"schedule,omitempty"`
	Prompt      *string    `json:"prompt,omitempty"`
	Paused      bool       `json:"paused"`
	LastRunAt   *wire.Time `json:"lastRunAt,omitempty"`
	NextRunAt   *wire.Time `json:"nextRunAt,omitempty"`
	UpdatedAt   *wire.Time `json:"updatedAt,omitempty"`
}

// JobsResponse is HermesMirroredJobsResponse.
type JobsResponse struct {
	Source string   `json:"source"`
	Jobs   []JobDTO `json:"jobs"`
}

// VaultImportResultDTO is HermesVaultImportResultDTO.
type VaultImportResultDTO struct {
	VaultPath string  `json:"vaultPath"`
	Scanned   int     `json:"scanned"`
	Imported  int     `json:"imported"`
	Skipped   int     `json:"skipped"`
	Failed    int     `json:"failed"`
	Truncated bool    `json:"truncated"`
	Cursor    *string `json:"cursor,omitempty"`
}

// VaultCreateResultDTO is HermesVaultCreateResultDTO.
type VaultCreateResultDTO struct {
	VaultPath          string   `json:"vaultPath"`
	AlreadyExisted     bool     `json:"alreadyExisted"`
	CreatedDirectories []string `json:"createdDirectories"`
	InstalledSkills    []string `json:"installedSkills"`
}

// SessionsImportResultDTO is HermesSessionsImportResultDTO.
type SessionsImportResultDTO struct {
	SessionsImported int     `json:"sessionsImported"`
	SessionsSkipped  int     `json:"sessionsSkipped"`
	FilesWritten     int     `json:"filesWritten"`
	Truncated        bool    `json:"truncated"`
	Cursor           *string `json:"cursor,omitempty"`
	CompileTriggered bool    `json:"compileTriggered"`
}

// CompileJobInstallResultDTO is HermesCompileJobInstallResultDTO.
type CompileJobInstallResultDTO struct {
	JobID    string `json:"jobID"`
	Schedule string `json:"schedule"`
	Created  bool   `json:"created"`
}

// TokensDTO is HermesJobRunTokensDTO.
type TokensDTO struct {
	Input  *int `json:"input,omitempty"`
	Output *int `json:"output,omitempty"`
}

// JobRunDTO is HermesJobRunDTO.
type JobRunDTO struct {
	ID            wire.UUID  `json:"id"`
	HermesJobID   string     `json:"hermesJobID"`
	RunKey        string     `json:"runKey"`
	Status        string     `json:"status"`
	StartedAt     wire.Time  `json:"startedAt"`
	FinishedAt    *wire.Time `json:"finishedAt,omitempty"`
	Output        *string    `json:"output,omitempty"`
	Error         *string    `json:"error,omitempty"`
	Tokens        *TokensDTO `json:"tokens,omitempty"`
	VaultFilePath *string    `json:"vaultFilePath,omitempty"`
	SkillRunLogID *wire.UUID `json:"skillRunLogID,omitempty"`
	CollectedAt   *wire.Time `json:"collectedAt,omitempty"`
}

// JobRunsResponse is HermesJobRunsResponse.
type JobRunsResponse struct {
	HermesJobID string      `json:"hermesJobID"`
	Runs        []JobRunDTO `json:"runs"`
	CollectedAt *wire.Time  `json:"collectedAt,omitempty"`
}

// CollectResultDTO is HermesJobCollectResultDTO.
type CollectResultDTO struct {
	HermesJobID   string     `json:"hermesJobID"`
	Fetched       int        `json:"fetched"`
	Inserted      int        `json:"inserted"`
	Skipped       int        `json:"skipped"`
	FilesWritten  int        `json:"filesWritten"`
	Truncated     bool       `json:"truncated"`
	HighWaterMark *wire.Time `json:"highWaterMark,omitempty"`
}

// ArtifactDTO is HermesArtifactDTO.
type ArtifactDTO struct {
	ID           wire.UUID `json:"id"`
	Kind         string    `json:"kind"`
	Value        string    `json:"value"`
	Href         string    `json:"href"`
	Label        string    `json:"label"`
	SessionID    string    `json:"sessionID"`
	SessionTitle string    `json:"sessionTitle"`
	OccurredAt   wire.Time `json:"occurredAt"`
}

// ArtifactListResponse is HermesArtifactListResponse.
type ArtifactListResponse struct {
	Artifacts  []ArtifactDTO `json:"artifacts"`
	NextCursor *string       `json:"nextCursor,omitempty"`
}

// Workspace DTOs (HermesWorkspace*DTO).
type (
	WorkspaceRepoDTO struct {
		Root       string  `json:"root"`
		Branch     *string `json:"branch,omitempty"`
		IsDetached bool    `json:"isDetached"`
	}
	WorkspaceBranchDTO struct {
		Name      string `json:"name"`
		IsCurrent bool   `json:"isCurrent"`
	}
	WorkspaceWorktreeDTO struct {
		Path      string  `json:"path"`
		Branch    *string `json:"branch,omitempty"`
		IsPrimary bool    `json:"isPrimary"`
	}
	WorkspaceChangeDTO struct {
		Path    string `json:"path"`
		Added   int    `json:"added"`
		Removed int    `json:"removed"`
		Status  string `json:"status"`
	}
	WorkspaceStatusDTO struct {
		Repo      WorkspaceRepoDTO       `json:"repo"`
		Branches  []WorkspaceBranchDTO   `json:"branches"`
		Worktrees []WorkspaceWorktreeDTO `json:"worktrees"`
		Changes   []WorkspaceChangeDTO   `json:"changes"`
	}
	WorkspaceDiffDTO struct {
		Path      string `json:"path"`
		Diff      string `json:"diff"`
		Truncated bool   `json:"truncated"`
	}
	WorkspaceFileEntryDTO struct {
		Name        string `json:"name"`
		Path        string `json:"path"`
		IsDirectory bool   `json:"isDirectory"`
	}
	WorkspaceListingDTO struct {
		Path    string                  `json:"path"`
		Entries []WorkspaceFileEntryDTO `json:"entries"`
	}
	WorkspaceFileDTO struct {
		Path      string `json:"path"`
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	}
)

// JobCreateRequest is HermesJobCreateRequest.
type JobCreateRequest struct {
	Name            string    `json:"name"`
	Schedule        string    `json:"schedule"`
	Prompt          *string   `json:"prompt,omitempty"`
	Deliver         *string   `json:"deliver,omitempty"`
	Skills          *[]string `json:"skills,omitempty"`
	Model           *string   `json:"model,omitempty"`
	Provider        *string   `json:"provider,omitempty"`
	BaseURL         *string   `json:"baseURL,omitempty"`
	Script          *string   `json:"script,omitempty"`
	ContextFrom     *[]string `json:"contextFrom,omitempty"`
	EnabledToolsets *[]string `json:"enabledToolsets,omitempty"`
	Workdir         *string   `json:"workdir,omitempty"`
	NoAgent         *bool     `json:"noAgent,omitempty"`
}

// JobUpdateRequest is HermesJobUpdateRequest.
type JobUpdateRequest struct {
	Name            *string   `json:"name,omitempty"`
	Schedule        *string   `json:"schedule,omitempty"`
	Prompt          *string   `json:"prompt,omitempty"`
	Deliver         *string   `json:"deliver,omitempty"`
	Skills          *[]string `json:"skills,omitempty"`
	Model           *string   `json:"model,omitempty"`
	Provider        *string   `json:"provider,omitempty"`
	BaseURL         *string   `json:"baseURL,omitempty"`
	Script          *string   `json:"script,omitempty"`
	ContextFrom     *[]string `json:"contextFrom,omitempty"`
	EnabledToolsets *[]string `json:"enabledToolsets,omitempty"`
	Workdir         *string   `json:"workdir,omitempty"`
	NoAgent         *bool     `json:"noAgent,omitempty"`
	Enabled         *bool     `json:"enabled,omitempty"`
}

// IsEmpty is HermesJobUpdateRequest.isEmpty.
func (r JobUpdateRequest) IsEmpty() bool {
	return r.Name == nil && r.Schedule == nil && r.Prompt == nil && r.Deliver == nil && r.Skills == nil &&
		r.Model == nil && r.Provider == nil && r.BaseURL == nil && r.Script == nil && r.ContextFrom == nil &&
		r.EnabledToolsets == nil && r.Workdir == nil && r.NoAgent == nil && r.Enabled == nil
}

func wtime(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}
