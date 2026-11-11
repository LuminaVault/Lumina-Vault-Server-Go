package workers

import (
	"context"
	"errors"
	"lumina/internal/cronexpr"
	"lumina/internal/jobs"
	"lumina/internal/memorystore"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ports Tests/AppTests/Memory/GraphLayoutServiceTests.swift: pure PCA projection,
// no database.

// Swift: "projects to bounded cube".
func TestProjectsToBoundedCube(t *testing.T) {
	vectors := make([][]float64, 50)
	for i := range vectors {
		vectors[i] = make([]float64, 16)
		for j := range 16 {
			vectors[i][j] = float64((i*7+j*3)%11) - 5.0
		}
	}
	coords := ProjectTo3D(vectors)
	require.Len(t, coords, len(vectors))
	for _, c := range coords {
		for k := range 3 {
			assert.LessOrEqual(t, math.Abs(c[k]), GraphCubeExtent+1e-6)
		}
	}
}

// Swift: "is deterministic for identical input".
func TestProjectionIsDeterministic(t *testing.T) {
	vectors := make([][]float64, 30)
	for i := range vectors {
		vectors[i] = make([]float64, 12)
		for j := range 12 {
			vectors[i][j] = math.Sin(float64(i*12 + j))
		}
	}
	assert.Equal(t, ProjectTo3D(vectors), ProjectTo3D(vectors))
}

// Swift: "handles fewer than two rows".
func TestProjectionHandlesFewerThanTwoRows(t *testing.T) {
	assert.Empty(t, ProjectTo3D(nil))
	one := ProjectTo3D([][]float64{{1, 2, 3}})
	require.Len(t, one, 1)
	assert.Equal(t, [3]float64{}, one[0])
}

// Swift: "separates two clusters along the principal axis".
func TestProjectionSeparatesTwoClusters(t *testing.T) {
	var vectors [][]float64
	for range 20 {
		vectors = append(vectors, make([]float64, 8))
	}
	for range 20 {
		v := make([]float64, 8)
		for j := range v {
			v[j] = 10
		}
		vectors = append(vectors, v)
	}
	coords := ProjectTo3D(vectors)
	var meanA, meanB float64
	for i := range 20 {
		meanA += coords[i][0] / 20
		meanB += coords[20+i][0] / 20
	}
	assert.Greater(t, math.Abs(meanA-meanB), 1.0)
}

// Swift: "parses pgvector text literal".
func TestParsesPgVectorTextLiteral(t *testing.T) {
	assert.Equal(t, []float64{1, 2, 3}, ParsePgVector("[1,2,3]"))
	assert.Equal(t, []float64{0.5, -0.25, 4}, ParsePgVector("[0.5, -0.25, 4]"))
	assert.Empty(t, ParsePgVector("[]"))
}

// Swift: "activity blends recency and score" (MemoryGraphService.activity, which Go
// keeps in memorystore).
func TestActivityBlendsRecencyAndScore(t *testing.T) {
	now := time.Now()
	stale := now.Add(-120 * day)
	fresh := memorystore.GraphActivity(50, &now, now)
	old := memorystore.GraphActivity(50, &stale, now)
	assert.Greater(t, fresh, old)
	assert.LessOrEqual(t, fresh, 1.0)
	assert.GreaterOrEqual(t, old, 0.0)
}

// RetrievalLeakReportWorkerTests "tick only fires at Sunday 05 UTC", the part that
// needs no database: 1970-01-04 was the first Sunday.
func TestLeakTickOnlyFiresSundayFiveUTC(t *testing.T) {
	w := &RetrievalLeakReportWorker{}
	monday5 := time.Unix(4*86400+5*3600, 0)
	sunday6 := time.Unix(3*86400+6*3600, 0)
	for _, at := range []time.Time{monday5, sunday6} {
		n, err := w.Tick(context.Background(), at)
		assert.Zero(t, n)
		assert.True(t, errors.Is(err, jobs.ErrSkipped), "a tick outside the hour is a skipped run")
	}
	sunday5 := time.Unix(3*86400+5*3600+59*60, 0).UTC()
	assert.Equal(t, 5, sunday5.Hour())
	assert.Equal(t, time.Sunday, sunday5.Weekday())
}

func TestWeeklyWindowEndsAtStartOfUTCDay(t *testing.T) {
	at := time.Date(2026, 11, 8, 5, 30, 0, 0, time.FixedZone("x", 3*3600))
	w := WeeklyWindow(at)
	assert.Equal(t, time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC), w.End)
	assert.Equal(t, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), w.Start)
}

func TestLeakInsightSummary(t *testing.T) {
	mean, worst := 0.4, "query"
	assert.Equal(t, "4 retrievals; 25% found nothing; avg best match 0.40; weakest path: query.",
		LeakInsightSummary(4, 0.25, &mean, &worst))
	assert.Equal(t, "2 retrievals; 0% found nothing.", LeakInsightSummary(2, 0, nil, &worst))
	assert.Equal(t, "8 retrievals; 13% found nothing.", LeakInsightSummary(8, 0.125, nil, nil), "rounds half away from zero")
}

func TestNextOccurrence(t *testing.T) {
	every, ok := cronexpr.Parse("* * * * *")
	require.True(t, ok)
	after := time.Date(2026, 1, 1, 10, 0, 30, 500, time.UTC)
	next, ok := NextOccurrence(every, after)
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 1, 1, 10, 1, 0, 500, time.UTC), next, "keeps the sub-second part, as Swift")

	daily, ok := cronexpr.Parse("0 9 * * *")
	require.True(t, ok)
	next, ok = NextOccurrence(daily, after)
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 1, 2, 9, 0, 0, 500, time.UTC), next)

	never, ok := cronexpr.Parse("0 0 31 2 *")
	require.True(t, ok)
	_, ok = NextOccurrence(never, after)
	assert.False(t, ok, "no match within 366 days falls back to one-shot")

	_, ok = cronexpr.Parse("* * *")
	assert.False(t, ok)
	_, ok = cronexpr.Parse("/ * * * *")
	assert.False(t, ok)
}
