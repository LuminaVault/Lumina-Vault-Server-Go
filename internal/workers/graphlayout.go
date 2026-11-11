package workers

import (
	"context"
	"fmt"
	"log/slog"
	"lumina/internal/jobs"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GraphLayout constants are GraphLayoutService's.
const (
	// GraphMaxLayoutNodes caps how many memories one tenant's layout holds; the
	// highest-score memories win.
	GraphMaxLayoutNodes = 2000
	// GraphCubeExtent is the half-extent of the output cube.
	GraphCubeExtent      = 60.0
	graphPowerIterations = 64
	graphConvergenceEps  = 1e-6
	graphUpdateBatch     = 250
)

// GraphLayoutWorker is GraphLayoutWorker (Sources/App/Workers/GraphLayoutWorker.swift)
// over GraphLayoutService (Sources/App/Memory/GraphLayoutService.swift): at boot and
// then hourly, recompute the 3D PCA layout of every tenant that has an embedded
// memory without coordinates.
//
// Swift quirk, kept: a tenant with more than GraphMaxLayoutNodes embedded memories
// always has some left without coordinates, so it is laid out again every hour.
type GraphLayoutWorker struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	Now    func() time.Time
}

// Job runs Tick at boot and then every hour.
func (w *GraphLayoutWorker) Job() jobs.Job {
	return jobs.Job{
		Name:     "GraphLayoutWorker",
		Schedule: jobs.Every(time.Hour, 0),
		Run: func(ctx context.Context) error {
			_, err := w.Tick(ctx)
			return err
		},
	}
}

func (w *GraphLayoutWorker) logger() *slog.Logger {
	if w.Logger == nil {
		return slog.Default()
	}
	return w.Logger
}

// Tick is GraphLayoutWorker.tick: one pass over all users. It returns how many
// tenants were laid out. A tenant's failure is logged and the pass goes on.
func (w *GraphLayoutWorker) Tick(ctx context.Context) (int, error) {
	tenants, err := allUserIDs(ctx, w.Pool)
	if err != nil {
		return 0, err
	}
	relaid := 0
	for _, tenant := range tenants {
		var unlaid int
		err := w.Pool.QueryRow(ctx, `
			SELECT COUNT(*)::int AS unlaid
			FROM memories
			WHERE tenant_id = $1 AND embedding IS NOT NULL AND graph_x IS NULL`, tenant).Scan(&unlaid)
		if err == nil && unlaid > 0 {
			var count int
			count, err = w.ComputeAndPersist(ctx, tenant)
			if err == nil && count > 0 {
				relaid++
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return relaid, ctx.Err()
			}
			w.logger().Warn(fmt.Sprintf("graph.layout tenant=%s error: %v", strings.ToUpper(tenant.String()), err))
		}
	}
	return relaid, nil
}

// ComputeAndPersist is GraphLayoutService.computeAndPersist: it writes graph_x/y/z
// and graph_layout_at and returns how many memories it laid out, 0 when the tenant
// has fewer than 2 embedded memories.
func (w *GraphLayoutWorker) ComputeAndPersist(ctx context.Context, tenant uuid.UUID) (int, error) {
	rows, err := w.Pool.Query(ctx, `
		SELECT id, embedding::text AS embedding_text
		FROM memories
		WHERE tenant_id = $1 AND embedding IS NOT NULL
		ORDER BY score DESC, last_accessed_at DESC NULLS LAST, id ASC
		LIMIT $2`, tenant, GraphMaxLayoutNodes)
	if err != nil {
		return 0, err
	}
	var ids []uuid.UUID
	var vectors [][]float64
	for rows.Next() {
		var id uuid.UUID
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			rows.Close()
			return 0, err
		}
		if v := ParsePgVector(text); len(v) > 0 {
			ids = append(ids, id)
			vectors = append(vectors, v)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) < 2 {
		return 0, nil
	}
	coords := ProjectTo3D(vectors)
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	for start := 0; start < len(ids); start += graphUpdateBatch {
		end := min(start+graphUpdateBatch, len(ids))
		xs, ys, zs := make([]float64, 0, end-start), make([]float64, 0, end-start), make([]float64, 0, end-start)
		for _, c := range coords[start:end] {
			xs, ys, zs = append(xs, c[0]), append(ys, c[1]), append(zs, c[2])
		}
		if _, err := w.Pool.Exec(ctx, `
			UPDATE memories AS m
			SET graph_x = v.x, graph_y = v.y, graph_z = v.z, graph_layout_at = $5
			FROM unnest($1::uuid[], $2::double precision[], $3::double precision[], $4::double precision[]) AS v(id, x, y, z)
			WHERE m.id = v.id AND m.tenant_id = $6`,
			ids[start:end], xs, ys, zs, now, tenant); err != nil {
			return 0, err
		}
	}
	w.logger().Info(fmt.Sprintf("laid out %d memories for tenant %s", len(ids), strings.ToUpper(tenant.String())))
	return len(ids), nil
}

// ParsePgVector is GraphLayoutService.parsePgVector: a pgvector text literal
// `[a,b,c]` to floats. Pieces that don't parse are dropped.
func ParsePgVector(text string) []float64 {
	trimmed := strings.Trim(text, "[] \n")
	if trimmed == "" {
		return nil
	}
	var out []float64
	for _, piece := range strings.Split(trimmed, ",") {
		if piece == "" {
			continue // split(separator:) drops empty pieces
		}
		if v, err := strconv.ParseFloat(strings.Trim(piece, " \t"), 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// ProjectTo3D is GraphLayoutService.projectTo3D: NIPALS power iteration for the top
// three principal components, then each axis scaled into the cube. The arithmetic
// keeps Swift's order, and every product is converted explicitly so the compiler
// can't fuse it into an FMA on arm64, which would change the last bits.
func ProjectTo3D(vectors [][]float64) [][3]float64 {
	n := len(vectors)
	out := make([][3]float64, n)
	if n < 2 || len(vectors[0]) == 0 {
		return out
	}
	d := len(vectors[0])
	mean := make([]float64, d)
	for _, v := range vectors {
		for j := range d {
			mean[j] += v[j]
		}
	}
	for j := range d {
		mean[j] /= float64(n)
	}
	x := make([][]float64, n)
	for i, row := range vectors {
		r := make([]float64, d)
		copy(r, row)
		for j := range d {
			r[j] -= mean[j]
		}
		x[i] = r
	}

	scores := make([][3]float64, n)
	components := min(3, min(n-1, d))
	for k := range components {
		t := column(x, k%d, n) // deterministic seed
		p := make([]float64, d)
		for range graphPowerIterations {
			tt := dot(t, t)
			if tt < graphConvergenceEps {
				break
			}
			p = matTvec(x, t, n, d)
			for j := range d {
				p[j] /= tt
			}
			normalize(p)
			tNew := matVec(x, p, n, d)
			delta := diffNorm(t, tNew)
			t = tNew
			if delta < graphConvergenceEps {
				break
			}
		}
		for i := range n {
			scores[i][k] = t[i]
		}
		for i := range n {
			ti := t[i]
			if ti == 0 {
				continue
			}
			for j := range d {
				x[i][j] -= float64(ti * p[j])
			}
		}
	}
	return normalizeToCube(scores)
}

// normalizeToCube is GraphLayoutService.normalizeToCube.
func normalizeToCube(scores [][3]float64) [][3]float64 {
	var maxAbs [3]float64
	for _, s := range scores {
		for k := range 3 {
			maxAbs[k] = math.Max(maxAbs[k], math.Abs(s[k]))
		}
	}
	var scale [3]float64
	for k, m := range maxAbs {
		if m > 1e-9 {
			scale[k] = GraphCubeExtent / m
		}
	}
	out := make([][3]float64, len(scores))
	for i, s := range scores {
		out[i] = [3]float64{float64(s[0] * scale[0]), float64(s[1] * scale[1]), float64(s[2] * scale[2])}
	}
	return out
}

func column(m [][]float64, index, rows int) []float64 {
	j := 0
	if len(m) > 0 {
		j = index % max(1, len(m[0]))
	}
	out := make([]float64, rows)
	for i := range rows {
		out[i] = m[i][j]
	}
	return out
}

func dot(a, b []float64) float64 {
	s := 0.0
	for i := range a {
		s += float64(a[i] * b[i])
	}
	return s
}

func matTvec(x [][]float64, t []float64, rows, cols int) []float64 {
	out := make([]float64, cols)
	for i := range rows {
		ti := t[i]
		if ti == 0 {
			continue
		}
		row := x[i]
		for j := range cols {
			out[j] += float64(row[j] * ti)
		}
	}
	return out
}

func matVec(x [][]float64, p []float64, rows, cols int) []float64 {
	out := make([]float64, rows)
	for i := range rows {
		s := 0.0
		row := x[i]
		for j := range cols {
			s += float64(row[j] * p[j])
		}
		out[i] = s
	}
	return out
}

func normalize(v []float64) {
	n := math.Sqrt(dot(v, v))
	if n > 1e-12 {
		for i := range v {
			v[i] /= n
		}
	}
}

func diffNorm(a, b []float64) float64 {
	s := 0.0
	for i := range a {
		d := a[i] - b[i]
		s += float64(d * d)
	}
	return math.Sqrt(s)
}

// allUserIDs is `User.query(on:).all()` reduced to the ids, in no particular order.
func allUserIDs(ctx context.Context, pool *pgxpool.Pool) ([]uuid.UUID, error) {
	rows, err := pool.Query(ctx, `SELECT id FROM users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
