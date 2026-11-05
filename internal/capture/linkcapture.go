// Package capture is the link-capture pipeline: LinkCaptureService,
// URLEnrichmentService, the enrichers and hooks, and the SSRF guard from
// LuminaVaultServer Sources/App/Capture. POST /v1/capture/safari
// (internal/modules/capture) is one caller. Import (wave-4 4F) and chat's
// auto-save-link (wave 5) are the others, which is why it isn't in the module.
//
// CaptureFilingService is filing.go: it needs LLM and is off in production.
package capture

//go:generate sh -c "cd ../.. && sqlc generate"

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"lumina/internal/achievementsink"
	"lumina/internal/capture/db"
	"lumina/internal/diskguard"
	"lumina/internal/ids"
	"lumina/internal/modules/achievements"
	"lumina/internal/onboardlatch"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CaptureLink's refusals, which the route answers with 400s.
var (
	ErrInvalidURL    = errors.New("capture: invalid URL")
	ErrNonPublicHost = errors.New("capture: URL host is not enrichable")
	ErrUnknownSpace  = errors.New("capture: space does not belong to the caller")
)

// CapturedLink is what CaptureLink saved.
type CapturedLink struct {
	FileID       uuid.UUID
	RelativePath string
}

// Options are LinkCaptureService's dependencies. Disk, Achievements and Latches
// may be nil.
type Options struct {
	Paths        vaultpaths.Paths
	Store        *vaultfiles.Store
	Enrichment   *EnrichmentService
	Disk         *diskguard.Guard
	Achievements *achievementsink.Sink
	Latches      *onboardlatch.Latches
	// Spawn runs enrichment, detached from the request. Tests wait on it. The
	// default is a goroutine: like Task.detached, it is lost if the process stops
	// mid-task, and the row then stays "pending".
	Spawn func(func())
	// Now defaults to time.Now.
	Now func() time.Time
}

// Service is LinkCaptureService.
type Service struct {
	q   *db.Queries
	opt Options
}

// New builds the service.
func New(pool *pgxpool.Pool, o Options) *Service {
	if o.Spawn == nil {
		o.Spawn = func(f func()) { go f() }
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{q: db.New(pool), opt: o}
}

// InstalledSlugsFrom is the read HookDispatcher.Run makes: the tenant's enabled
// plugin installs.
func InstalledSlugsFrom(pool *pgxpool.Pool) InstalledSlugs {
	q := db.New(pool)
	return func(ctx context.Context, tenant uuid.UUID) ([]string, error) {
		return q.EnabledPluginSlugs(ctx, tenant)
	}
}

// CaptureLink saves one URL to the tenant's vault as a placeholder note, answers
// with its location and enriches it in the background.
//
// relative is `<space slug or "inbox">/<timestamp>-<host>-<8 hex>.md`, built from
// a stored slug and the URL's decoded host and passed to ResolveInside with no
// SanitizePath, as in Swift. A host holding %2F therefore makes subdirectories.
// Dots become dashes, so it can't climb out with "..".
func (s *Service) CaptureLink(ctx context.Context, tenant uuid.UUID, urlString string, note *string, spaceID *uuid.UUID) (CapturedLink, error) {
	u, ok := ParseURL(urlString)
	if !ok {
		return CapturedLink{}, ErrInvalidURL
	}
	if !IsPublic(u) {
		return CapturedLink{}, ErrNonPublicHost
	}
	spaceSlug := "inbox"
	if spaceID != nil {
		slug, err := s.q.SpaceSlug(ctx, db.SpaceSlugParams{ID: *spaceID, TenantID: tenant})
		if errors.Is(err, pgx.ErrNoRows) {
			return CapturedLink{}, ErrUnknownSpace
		}
		if err != nil {
			return CapturedLink{}, err
		}
		spaceSlug = slug
	}

	data := []byte(placeholderMarkdown(urlString, note))
	host := "link"
	if u.Host != nil {
		host = *u.Host
	}
	suffix := strings.ToLower(uuid.NewString()[:8])
	relative := spaceSlug + "/" + s.opt.Now().Format("2006-01-02-150405") + "-" + strings.ReplaceAll(host, ".", "-") + "-" + suffix + ".md"

	if s.opt.Disk != nil {
		if err := s.opt.Disk.Check(int64(len(data))); err != nil {
			return CapturedLink{}, err
		}
	}
	if err := s.opt.Paths.EnsureTenantDirectories(tenant); err != nil {
		return CapturedLink{}, err
	}
	target, err := vaultfiles.ResolveInside(s.opt.Paths.Raw(tenant), relative)
	if err != nil {
		return CapturedLink{}, err
	}
	if err := vaultfiles.CreateParent(target); err != nil {
		return CapturedLink{}, err
	}
	if err := vaultfiles.WriteAtomic(target, data); err != nil {
		return CapturedLink{}, err
	}

	sum := sha256.Sum256(data)
	pending := "pending"
	fileID, err := s.opt.Store.Upsert(ctx, vaultfiles.UpsertParams{
		TenantID: tenant, SpaceID: spaceID, Path: relative, ContentType: "text/markdown",
		SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]),
		Metadata: &vaultfiles.Metadata{EnrichmentStatus: &pending},
	})
	if err != nil {
		return CapturedLink{}, err
	}
	slog.InfoContext(ctx, "link captured", "tenant", ids.Upper(tenant), "url", urlString, "path", relative)

	// EventBus vault_file_created has only a log-only subscriber, so it's dropped
	// (wave-4-plan.md, "Channels").
	s.opt.Achievements.Enqueue(tenant, achievements.EventVaultUploaded)
	s.opt.Latches.Latch(ctx, onboardlatch.FirstCapture, tenant)

	s.opt.Spawn(func() {
		s.opt.Enrichment.EnrichAndRewrite(context.Background(), fileID, urlString, tenant)
	})
	return CapturedLink{FileID: fileID, RelativePath: relative}, nil
}

// placeholderMarkdown is the file until enrichment rewrites it.
func placeholderMarkdown(urlString string, note *string) string {
	markdown := "---\nsource: \"" + urlString + "\"\n---\n\n# [Pending Enrichment]\nURL: " + urlString + "\n"
	if note != nil && *note != "" {
		markdown += "\n## Note\n" + *note + "\n"
	}
	return markdown
}
