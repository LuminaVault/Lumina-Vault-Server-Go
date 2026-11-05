package ingestion

import (
	"encoding/json"
	"lumina/internal/config"
	"lumina/internal/wire"
	"math"
	"net/http"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// Swift StorageQuotaServiceTests.

func TestEachTierGetsItsOwnCeiling(t *testing.T) {
	limits := DefaultLimits()
	require.EqualValues(t, 5*1024*1024*1024, *limits.For("trial"))
	require.EqualValues(t, 100*1024*1024*1024, *limits.For("pro"))
	require.EqualValues(t, int64(1024)*1024*1024*1024, *limits.For("ultimate"))
}

func TestLapsedAndArchivedCannotGrow(t *testing.T) {
	limits := DefaultLimits()
	require.Zero(t, *limits.For("lapsed"))
	require.Zero(t, *limits.For("archived"))
	require.Zero(t, *limits.For("not a tier"), "User.tierEnum reads unknown text as lapsed")
}

func TestFreeHasARealCeilingItCanGrowInto(t *testing.T) {
	limits := DefaultLimits()
	free, trial := *limits.For("free"), *limits.For("trial")
	require.EqualValues(t, 1024*1024*1024, free)
	require.Positive(t, free)
	require.Less(t, free, trial)
}

func TestSizesFormatAtTheUnitAHumanWouldUse(t *testing.T) {
	for n, want := range map[int64]string{
		0:                         "0 B",
		512:                       "512 B",
		2 * 1024 * 1024:           "2.0 MiB",
		5 * 1024 * 1024 * 1024:    "5.0 GiB",
		1024 * 1024 * 1024 * 1024: "1.0 TiB",
		1024:                      "1.0 KiB",
		1073741000:                "1024.0 MiB",
	} {
		require.Equal(t, want, formatBytes(n))
	}
}

func TestDenialMessageCarriesUsedAndLimit(t *testing.T) {
	require.Equal(t, "storage quota exceeded: 5.0 GiB of 100.0 GiB used", quotaMessage(5*1024*1024*1024, 100*1024*1024*1024))
}

func TestUnlimitedAndNoGrowthAreDifferentValues(t *testing.T) {
	unlimited := Limits{}
	require.Nil(t, unlimited.For("trial"))
	limits := DefaultLimits()
	require.NotNil(t, limits.For("lapsed"))
	require.Zero(t, *limits.For("lapsed"))
}

func TestConfigurationMapsOntoThatSentinel(t *testing.T) {
	require.Nil(t, LimitFrom(-1))
	require.Zero(t, *LimitFrom(0))
	require.EqualValues(t, 1024, *LimitFrom(1024))
}

func TestQuotaFromConfigKeepsLapsedAtZero(t *testing.T) {
	q := QuotaFromConfig(config.Config{StorageQuotaEnabled: true, StorageQuotaTrialBytes: -1, StorageQuotaProBytes: 2, StorageQuotaUltimateBytes: 3, StorageQuotaFreeBytes: 4})
	require.True(t, q.Enabled)
	require.Nil(t, q.Limits.Trial)
	require.EqualValues(t, 2, *q.Limits.Pro)
	require.EqualValues(t, 3, *q.Limits.Ultimate)
	require.EqualValues(t, 4, *q.Limits.Free)
	require.Zero(t, *q.Limits.Lapsed)
}

func TestParseRange(t *testing.T) {
	type r struct{ first, last int64 }
	const size = 100
	for value, want := range map[string]*r{
		"bytes=0-9":     {0, 9},
		"bytes=7-7":     {7, 7},
		"bytes=90-":     {90, 99},
		"bytes=90-500":  {90, 99},
		"bytes=-10":     {90, 99},
		"bytes=-500":    {0, 99},
		"bytes=+1-+2":   {1, 2},
		"bytes=100-":    nil,
		"bytes=5-2":     nil,
		"bytes=0-x":     nil,
		"bytes=x-1":     nil,
		"bytes=-0":      nil,
		"bytes=-":       nil,
		"bytes=-x":      nil,
		"bytes=":        nil,
		"bytes=0-1,2-3": nil,
		"bytes=0-1-2":   nil,
		"items=0-1":     nil,
		"":              nil,
	} {
		first, last, err := parseRange(value, size)
		if want == nil {
			require.Equal(t, http.StatusRequestedRangeNotSatisfiable, echo.StatusCode(err), value)
			continue
		}
		require.NoError(t, err, value)
		require.Equal(t, *want, r{first, last}, value)
	}
	_, _, err := parseRange("bytes=0-1", 0)
	require.Error(t, err, "an empty file has no range")
}

func TestBatchState(t *testing.T) {
	require.Equal(t, "completed", batchState(nil), "allSatisfy on nothing")
	require.Equal(t, "completed", batchState([]string{"completed", "completed"}))
	for _, other := range []string{"failed", "blocked_capability", "cancelled"} {
		require.Equal(t, "attention", batchState([]string{"completed", other}))
	}
	for _, open := range []string{"awaiting_upload", "queued", "extracting", "analyzing", "saving", "weird"} {
		require.Equal(t, "active", batchState([]string{"completed", open}))
	}
}

func TestIsHTTPURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://example.test/a?b=c#d": true,
		"HTTP://EXAMPLE.test":          true,
		"https://example.test/a b/é":   true,
		"http:":                        true,
		"":                             false,
		"example.test":                 false,
		"ftp://example.test":           false,
		"file:///etc/passwd":           false,
		"http://a:b/":                  false,
		"http://[x":                    false,
	} {
		require.Equal(t, want, isHTTPURL(raw), raw)
	}
}

func TestDeclaredBytesCountsEveryItemAndSaturates(t *testing.T) {
	size := func(n wire.Int64) *wire.Int64 { return &n }
	require.Zero(t, declaredBytes([]createItemRequest{{}, {Kind: kindURL}}))
	require.EqualValues(t, 3, declaredBytes([]createItemRequest{{SizeBytes: size(1)}, {}, {Kind: kindURL, SizeBytes: size(2)}}))
	require.EqualValues(t, -1, declaredBytes([]createItemRequest{{SizeBytes: size(-1)}}))
	require.EqualValues(t, math.MaxInt64, declaredBytes([]createItemRequest{{SizeBytes: size(math.MaxInt64)}, {SizeBytes: size(1)}}))
	require.EqualValues(t, math.MinInt64, declaredBytes([]createItemRequest{{SizeBytes: size(math.MinInt64)}, {SizeBytes: size(-1)}}))
}

func TestSourceTokenHashIsLowerHexOfTheBytes(t *testing.T) {
	require.Equal(t, "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824", sourceTokenHash("hello"))
}

func TestItemStateAndEventTypeKnowTheEnums(t *testing.T) {
	for _, s := range []string{"awaiting_upload", "queued", "extracting", "analyzing", "saving", "blocked_capability", "completed", "failed", "cancelled"} {
		got, ok := itemState(s)
		require.True(t, ok, s)
		require.Equal(t, s, got)
	}
	_, ok := itemState("weird")
	require.False(t, ok)
	for _, s := range []string{"snapshot", "state_changed", "progress", "terminal", "heartbeat"} {
		require.True(t, eventType(s), s)
	}
	require.False(t, eventType("weird"))
}

func TestCreateRequestDecodesAsFoundationDoes(t *testing.T) {
	var k sourceKind
	require.NoError(t, json.Unmarshal([]byte(`"file"`), &k))
	var corrupted *wire.CorruptedError
	require.ErrorAs(t, json.Unmarshal([]byte(`"audio"`), &k), &corrupted)
	require.Equal(t, "Cannot initialize IngestionSourceKindDTO from invalid String value audio", corrupted.Message)
	var typeErr *json.UnmarshalTypeError
	require.ErrorAs(t, json.Unmarshal([]byte(`1`), &k), &typeErr)
}
