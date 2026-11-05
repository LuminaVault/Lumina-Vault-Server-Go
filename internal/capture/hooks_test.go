package capture

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type stubHook struct {
	binding string
	point   HookPoint
	apply   func(HookContext) (HookContext, error)
}

func (s stubHook) Binding() string  { return s.binding }
func (s stubHook) Point() HookPoint { return s.point }
func (s stubHook) Apply(_ context.Context, hc HookContext) (HookContext, error) {
	return s.apply(hc)
}

func ptr[T any](v T) *T { return &v }

func hookContext(description string) HookContext {
	return HookContext{
		Tenant: uuid.New(), URL: "https://example.com/article", Config: map[string]string{},
		Metadata: EnrichedMetadata{Title: ptr("Example"), Description: &description, URL: "https://example.com/article"},
	}
}

func dispatcher(hooks ...Hook) *HookDispatcher {
	return &HookDispatcher{Registry: NewHookRegistry(hooks...)}
}

var fourHundredWords = strings.Repeat("lorem ", 400)

// CaptureHookDispatcherTests.swift.
func TestReadingTimeTransformsAtPostEnrich(t *testing.T) {
	got := dispatcher(ReadingTimeHook{}).Dispatch(context.Background(), PostEnrich, []string{"reading-time"}, hookContext(fourHundredWords))
	require.Equal(t, 2, *got.Metadata.ReadingTimeMinutes)
}

func TestNotInstalledLeavesMetadata(t *testing.T) {
	got := dispatcher(ReadingTimeHook{}).Dispatch(context.Background(), PostEnrich, nil, hookContext(fourHundredWords))
	require.Nil(t, got.Metadata.ReadingTimeMinutes)
}

func TestNonCaptureHookInstallIgnored(t *testing.T) {
	got := dispatcher(ReadingTimeHook{}).Dispatch(context.Background(), PostEnrich, []string{"readwise"}, hookContext(fourHundredWords))
	require.Nil(t, got.Metadata.ReadingTimeMinutes)
}

func TestFailingHookIsIsolated(t *testing.T) {
	failing := stubHook{"reading-time", PostEnrich, func(HookContext) (HookContext, error) { return HookContext{}, errors.New("boom") }}
	got := dispatcher(failing).Dispatch(context.Background(), PostEnrich, []string{"reading-time"}, hookContext("untouched"))
	require.Equal(t, "untouched", *got.Metadata.Description)
	require.Nil(t, got.Metadata.ReadingTimeMinutes)
}

func TestHookForOtherPointSkipped(t *testing.T) {
	other := stubHook{"reading-time", BeforePersist, func(hc HookContext) (HookContext, error) {
		hc.Metadata.ReadingTimeMinutes = ptr(999)
		return hc, nil
	}}
	got := dispatcher(other).Dispatch(context.Background(), PostEnrich, []string{"reading-time"}, hookContext("x"))
	require.Nil(t, got.Metadata.ReadingTimeMinutes)
}

// The half of "reading-time is a captureHook entry in the catalog" that lives here.
// The rest is in internal/modules/marketplace.
func TestReadingTimeBindingInCatalog(t *testing.T) {
	require.Equal(t, map[string]string{"reading-time": "reading-time"}, captureHookBindings)
}

func TestReadingTimeRoundsUpAndSkipsEmpty(t *testing.T) {
	apply := func(md EnrichedMetadata) *int {
		got, err := ReadingTimeHook{}.Apply(context.Background(), HookContext{Metadata: md})
		require.NoError(t, err)
		return got.Metadata.ReadingTimeMinutes
	}
	require.Nil(t, apply(EnrichedMetadata{}))
	require.Nil(t, apply(EnrichedMetadata{Body: ptr(" \n\t ")}))
	require.Equal(t, 1, *apply(EnrichedMetadata{Body: ptr("one")}))
	require.Equal(t, 2, *apply(EnrichedMetadata{Description: ptr(strings.Repeat("a ", 200)), Body: ptr("b")}))
}

func TestRunWithFailedReadLeavesContext(t *testing.T) {
	d := &HookDispatcher{Registry: NewHookRegistry(ReadingTimeHook{}), Installed: func(context.Context, uuid.UUID) ([]string, error) {
		return nil, errors.New("db down")
	}}
	require.Nil(t, d.Run(context.Background(), PostEnrich, hookContext(fourHundredWords)).Metadata.ReadingTimeMinutes)
	d.Installed = func(context.Context, uuid.UUID) ([]string, error) { return []string{"reading-time"}, nil }
	require.Equal(t, 2, *d.Run(context.Background(), PostEnrich, hookContext(fourHundredWords)).Metadata.ReadingTimeMinutes)
}
