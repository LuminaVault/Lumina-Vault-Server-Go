package capture

import (
	"context"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/swiftstr"
	"math"
	"sort"
	"unicode"

	"github.com/google/uuid"
)

// HookPoint is CaptureHookPoint: where in the pipeline a hook runs.
type HookPoint string

const (
	// PostEnrich runs after enrichment, before the markdown is rendered. It's the
	// only point dispatched.
	PostEnrich HookPoint = "post_enrich"
	// BeforePersist is reserved. Nothing dispatches it.
	BeforePersist HookPoint = "before_persist"
)

// HookContext is CaptureHookContext, the payload a hook transforms.
type HookContext struct {
	Tenant uuid.UUID
	URL    string
	// Config is empty for now: no hook takes configuration.
	Config   map[string]string
	Metadata EnrichedMetadata
}

// Hook is CaptureHook, resolved by the catalog's binding.
type Hook interface {
	Binding() string
	Point() HookPoint
	Apply(ctx context.Context, hc HookContext) (HookContext, error)
}

// HookRegistry is CaptureHookRegistry.
type HookRegistry map[string]Hook

// NewHookRegistry maps hooks by binding. A later hook replaces an earlier one
// with the same binding.
func NewHookRegistry(hooks ...Hook) HookRegistry {
	r := HookRegistry{}
	for _, h := range hooks {
		r[h.Binding()] = h
	}
	return r
}

// captureHookBindings is the subset of PluginCatalog with capabilityKind
// .captureHook: slug to binding. Any other slug, such as a connector, is ignored
// by Dispatch. internal/modules/marketplace holds the browsable catalog.
var captureHookBindings = map[string]string{"reading-time": "reading-time"}

// InstalledSlugs reads the slugs of a tenant's enabled plugin installs.
type InstalledSlugs func(ctx context.Context, tenant uuid.UUID) ([]string, error)

// HookDispatcher is CaptureHookDispatcher.
type HookDispatcher struct {
	Registry  HookRegistry
	Installed InstalledSlugs
}

// Run reads the tenant's installs, then dispatches. A failed read is logged and
// leaves the context unchanged.
func (d *HookDispatcher) Run(ctx context.Context, point HookPoint, hc HookContext) HookContext {
	if d.Installed == nil {
		return hc
	}
	slugs, err := d.Installed(ctx, hc.Tenant)
	if err != nil {
		slog.ErrorContext(ctx, "capture-hook dispatch query failed", "tenant", ids.Upper(hc.Tenant), "point", string(point), "error", err)
		return hc
	}
	return d.Dispatch(ctx, point, slugs, hc)
}

// Dispatch runs the capture hooks among slugs that match point, in slug order. A
// hook that fails is logged and skipped: a plugin must never break a capture.
func (d *HookDispatcher) Dispatch(ctx context.Context, point HookPoint, slugs []string, hc HookContext) HookContext {
	sorted := append([]string(nil), slugs...)
	sort.Strings(sorted)
	for _, slug := range sorted {
		binding, ok := captureHookBindings[slug]
		if !ok {
			continue
		}
		hook, ok := d.Registry[binding]
		if !ok || hook.Point() != point {
			continue
		}
		next, err := hook.Apply(ctx, hc)
		if err != nil {
			slog.WarnContext(ctx, "capture-hook failed; skipping", "slug", slug, "point", string(point), "tenant", ids.Upper(hc.Tenant), "error", err)
			continue
		}
		hc = next
	}
	return hc
}

// wordsPerMinute is ReadingTimeHook.wordsPerMinute.
const wordsPerMinute = 200

// ReadingTimeHook estimates reading time from the enriched text. Deterministic,
// with no network and no configuration.
type ReadingTimeHook struct{}

func (ReadingTimeHook) Binding() string  { return "reading-time" }
func (ReadingTimeHook) Point() HookPoint { return PostEnrich }

func (ReadingTimeHook) Apply(_ context.Context, hc HookContext) (HookContext, error) {
	text := ""
	first := true
	for _, part := range []*string{hc.Metadata.Description, hc.Metadata.Transcript, hc.Metadata.Body} {
		if part == nil {
			continue
		}
		if !first {
			text += " "
		}
		text += *part
		first = false
	}
	words := countWords(text)
	if words == 0 {
		return hc, nil
	}
	minutes := int(math.Ceil(float64(words) / wordsPerMinute))
	hc.Metadata.ReadingTimeMinutes = &minutes
	return hc, nil
}

// countWords is `text.split { $0.isWhitespace || $0.isNewline }.count`. Swift
// splits on Characters, and a Character is whitespace when its first scalar is.
func countWords(text string) int {
	count := 0
	inWord := false
	for _, c := range swiftstr.Characters(text) {
		var first rune
		for _, r := range c {
			first = r
			break
		}
		if unicode.Is(unicode.White_Space, first) {
			inWord = false
			continue
		}
		if !inWord {
			count++
			inWord = true
		}
	}
	return count
}
