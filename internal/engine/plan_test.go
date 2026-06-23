package engine

import (
	"testing"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/sqlparse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPlanResolvesBindParam verifies a :name bind RHS is resolved against the
// params map and pushed to the connector as a literal-valued filter (so sources
// that require a filter value at fetch time receive a saved query's argument).
func TestPlanResolvesBindParam(t *testing.T) {
	q, err := sqlparse.Parse("SELECT * FROM github.issues WHERE owner = :owner")
	require.NoError(t, err)
	ts := source.TableSchema{Name: "issues", Columns: []source.Column{{Name: "owner"}}}

	req := planScan(q.Stmt, q.Stmt.From[0], ts, map[string]any{"owner": "golang"})
	assert.Equal(t, []source.Filter{{Column: "owner", Op: sqlparse.OpEq, Value: "golang"}}, req.Filters)
}

// An unbound parameter is not pushable: it is left for the local SQLite engine
// (which will error on the missing bind), never sent as an empty filter.
func TestPlanUnboundParamNotPushed(t *testing.T) {
	q, err := sqlparse.Parse("SELECT * FROM github.issues WHERE owner = :owner")
	require.NoError(t, err)
	ts := source.TableSchema{Name: "issues", Columns: []source.Column{{Name: "owner"}}}

	req := planScan(q.Stmt, q.Stmt.From[0], ts, nil)
	assert.Empty(t, req.Filters)
}

func planFor(t *testing.T, sql string) source.ScanRequest {
	t.Helper()
	q, err := sqlparse.Parse(sql)
	require.NoError(t, err)
	require.NotEmpty(t, q.Stmt.From)
	src := q.Stmt.From[0]
	ts := source.TableSchema{Name: src.Name, Columns: []source.Column{
		{Name: "owner"}, {Name: "repo"}, {Name: "state"}, {Name: "updated_at"}, {Name: "comments"},
	}}
	return planScan(q.Stmt, src, ts, nil)
}

// planForJoin plans the scan for the source at fromIdx, giving it a table schema
// with the named columns.
func planForJoin(t *testing.T, sql string, fromIdx int, cols ...string) source.ScanRequest {
	t.Helper()
	q, err := sqlparse.Parse(sql)
	require.NoError(t, err)
	require.Greater(t, len(q.Stmt.From), fromIdx)
	src := q.Stmt.From[fromIdx]
	columns := make([]source.Column, len(cols))
	for i, c := range cols {
		columns[i] = source.Column{Name: c}
	}
	return planScan(q.Stmt, src, source.TableSchema{Name: src.Name, Columns: columns}, nil)
}

// The driving table of an FK/dimension-lookup join (every other source pinned to
// constants via the join keys) gets LIMIT + ORDER pushed.
func TestPlanScanPushesLimitForFKLookupJoin(t *testing.T) {
	sql := `SELECT p.number FROM github.pulls p
		JOIN github.repos r ON r.owner = p.owner AND r.name = p.repo
		WHERE p.owner = 'golang' AND p.repo = 'go' AND p.state = 'open'
		ORDER BY p.updated_at DESC LIMIT 3`

	req := planForJoin(t, sql, 0, "owner", "repo", "state", "updated_at", "number")
	assert.Equal(t, []source.OrderTerm{{Column: "updated_at", Desc: true}}, req.OrderBy)
	require.NotNil(t, req.Limit)
	assert.Equal(t, 3, *req.Limit)
}

// The non-driving (lookup) source of that same join must NOT get the LIMIT.
func TestPlanScanLimitNotPushedToLookupSource(t *testing.T) {
	sql := `SELECT p.number FROM github.pulls p
		JOIN github.repos r ON r.owner = p.owner AND r.name = p.repo
		WHERE p.owner = 'golang' AND p.repo = 'go' AND p.state = 'open'
		ORDER BY p.updated_at DESC LIMIT 3`

	req := planForJoin(t, sql, 1, "owner", "name") // repos
	assert.Nil(t, req.Limit)
}

// A LEFT JOIN preserves the leftmost (driving) source's rows, so LIMIT is safe.
func TestPlanScanPushesLimitForLeftJoin(t *testing.T) {
	sql := `SELECT a.t FROM s.a a LEFT JOIN s.b b ON a.x = b.y
		WHERE a.k = 'v' ORDER BY a.t DESC LIMIT 3`

	req := planForJoin(t, sql, 0, "t", "k", "x")
	require.NotNil(t, req.Limit)
	assert.Equal(t, 3, *req.Limit)
}

// An INNER join on an unpinned key can drop driving rows, so LIMIT must NOT push.
func TestPlanScanNoLimitWhenJoinKeyUnpinned(t *testing.T) {
	sql := `SELECT a.t FROM s.a a JOIN s.b b ON a.x = b.y
		WHERE a.k = 'v' ORDER BY a.t DESC LIMIT 3`

	req := planForJoin(t, sql, 0, "t", "k", "x")
	assert.Nil(t, req.Limit)
}

// ORDER BY spanning two sources can't be honored by one source, so no LIMIT push.
func TestPlanScanNoLimitWhenOrderSpansSources(t *testing.T) {
	sql := `SELECT a.t FROM s.a a JOIN s.b b ON a.k = b.k
		WHERE a.k = 'v' ORDER BY a.t, b.u LIMIT 3`

	req := planForJoin(t, sql, 0, "t", "k")
	assert.Nil(t, req.Limit)
}

// RIGHT/FULL joins can drop or NULL-extend the driving source — never push LIMIT.
func TestPlanScanNoLimitWithRightJoin(t *testing.T) {
	sql := `SELECT a.t FROM s.a a RIGHT JOIN s.b b ON a.k = b.k
		WHERE a.k = 'v' ORDER BY a.t LIMIT 3`

	req := planForJoin(t, sql, 0, "t", "k")
	assert.Nil(t, req.Limit)
}

// A NATURAL JOIN's equi-condition never lands in Join.On, so the planner can't
// see it; its implicit INNER match can drop driving rows, so LIMIT must NOT push.
func TestPlanScanNoLimitWithNaturalJoin(t *testing.T) {
	sql := `SELECT a.t FROM s.a a NATURAL JOIN s.b b
		ORDER BY a.t DESC LIMIT 5`

	req := planForJoin(t, sql, 0, "t", "k")
	assert.Nil(t, req.Limit)
}

// Same for a USING(...) join — the join columns aren't in Join.On either.
func TestPlanScanNoLimitWithUsingJoin(t *testing.T) {
	sql := `SELECT a.t FROM s.a a JOIN s.b b USING (k)
		ORDER BY a.t DESC LIMIT 5`

	req := planForJoin(t, sql, 0, "t", "k")
	assert.Nil(t, req.Limit)
}

func TestPlanScanFiltersAndOrder(t *testing.T) {
	req := planFor(t, "SELECT * FROM github.issues WHERE owner='golang' AND state='open' AND comments > 5 ORDER BY updated_at DESC LIMIT 10")

	assert.ElementsMatch(t, []source.Filter{
		{Column: "owner", Op: sqlparse.OpEq, Value: "golang"},
		{Column: "state", Op: sqlparse.OpEq, Value: "open"},
		{Column: "comments", Op: sqlparse.OpGt, Value: int64(5)},
	}, req.Filters)
	assert.Equal(t, []source.OrderTerm{{Column: "updated_at", Desc: true}}, req.OrderBy)
	require.NotNil(t, req.Limit)
	assert.Equal(t, 10, *req.Limit)
}

// Projection push-down: a simple column SELECT narrows req.Columns to every
// column the source is referenced by (projection + WHERE + ORDER BY).
func TestPlanScanProjectsReferencedColumns(t *testing.T) {
	req := planFor(t, "SELECT owner, state FROM github.issues WHERE comments > 5 ORDER BY updated_at DESC")
	assert.Equal(t, []string{"comments", "owner", "state", "updated_at"}, req.Columns)
}

// SELECT * needs every column, so projection falls back to nil (= all).
func TestPlanScanStarProjectsAll(t *testing.T) {
	req := planFor(t, "SELECT * FROM github.issues WHERE owner='x'")
	assert.Nil(t, req.Columns)
}

// An aggregate/expression projection may reference columns we can't see -> all.
func TestPlanScanAggregateProjectsAll(t *testing.T) {
	req := planFor(t, "SELECT count(*) FROM github.issues WHERE owner='x'")
	assert.Nil(t, req.Columns)
}

// An unqualified SELECT * in a multi-source join expands to every source, so each
// source needs all its columns — projection must NOT narrow (else SQLite's * would
// return NULLs for the omitted columns).
func TestPlanScanStarMultiSourceProjectsAll(t *testing.T) {
	sql := `SELECT * FROM s.a a JOIN s.b b ON a.k = b.k WHERE a.t > 1`
	assert.Nil(t, planForJoin(t, sql, 0, "t", "k").Columns) // driving source a
	assert.Nil(t, planForJoin(t, sql, 1, "k", "v").Columns) // joined source b
}

// An unqualified column in a multi-source query could belong to this source, so
// attribution is ambiguous and projection falls back to nil (= all).
func TestPlanScanProjectionAmbiguousMultiSource(t *testing.T) {
	sql := `SELECT a.t FROM s.a a JOIN s.b b ON a.k = b.k WHERE k = 'v'`
	req := planForJoin(t, sql, 0, "t", "k")
	assert.Nil(t, req.Columns)
}

// Fully-qualified columns in a join narrow to the source's referenced set.
func TestPlanScanProjectionQualifiedMultiSource(t *testing.T) {
	sql := `SELECT a.t FROM s.a a JOIN s.b b ON a.k = b.k WHERE a.t > 1`
	req := planForJoin(t, sql, 0, "t", "k")
	assert.Equal(t, []string{"k", "t"}, req.Columns)
}

func TestPlanScanSkipsBindAndUnknownColumns(t *testing.T) {
	req := planFor(t, "SELECT * FROM github.issues WHERE owner = :o AND missing_col = 'x'")
	// :o is a bind (not pushable); missing_col is not in the table schema.
	assert.Empty(t, req.Filters)
}

func TestPlanScanLimitNotPushedForMultiSource(t *testing.T) {
	q, err := sqlparse.Parse(
		"SELECT * FROM github.issues i JOIN github.repos r ON i.repo = r.name WHERE i.owner = 'golang' LIMIT 5",
	)
	require.NoError(t, err)

	ts := source.TableSchema{Name: "issues", Columns: []source.Column{{Name: "owner"}, {Name: "repo"}}}
	req := planScan(q.Stmt, q.Stmt.From[0], ts, nil)
	assert.Nil(t, req.Limit, "LIMIT must not be pushed to one source of a multi-source query")
}

func TestPlanScanInfersJoinPartnerFilters(t *testing.T) {
	// repos has no direct owner/name filter; they are inferred from the join keys
	// r.owner=i.owner / r.name=i.repo plus issues' literal filters.
	q, err := sqlparse.Parse(`
		SELECT i.number, r.full_name
		FROM github.issues i
		JOIN github.repos r ON r.owner = i.owner AND r.name = i.repo
		WHERE i.owner = 'golang' AND i.repo = 'go' AND i.state = 'open'`)
	require.NoError(t, err)

	reposSrc := q.Stmt.From[1] // r
	require.Equal(t, "repos", reposSrc.Name)
	reposTS := source.TableSchema{Name: "repos", Columns: []source.Column{{Name: "owner"}, {Name: "name"}, {Name: "full_name"}}}

	req := planScan(q.Stmt, reposSrc, reposTS, nil)
	assert.ElementsMatch(t, []source.Filter{
		{Column: "owner", Op: sqlparse.OpEq, Value: "golang"},
		{Column: "name", Op: sqlparse.OpEq, Value: "go"},
	}, req.Filters)
}

func TestPlanScanInferenceDoesNotDuplicateDirectFilter(t *testing.T) {
	q, err := sqlparse.Parse(`
		SELECT * FROM github.issues i
		JOIN github.repos r ON r.name = i.repo
		WHERE i.repo = 'go' AND r.name = 'go'`)
	require.NoError(t, err)

	reposTS := source.TableSchema{Name: "repos", Columns: []source.Column{{Name: "name"}}}
	req := planScan(q.Stmt, q.Stmt.From[1], reposTS, nil)
	// Only one filter on name, even though it's both direct and inferable.
	assert.Len(t, req.Filters, 1)
}

func TestPlanScanQualifiedColumnAttribution(t *testing.T) {
	q, err := sqlparse.Parse("SELECT * FROM github.issues i WHERE i.owner = 'golang' AND i.state = 'open'")
	require.NoError(t, err)
	ts := source.TableSchema{Name: "issues", Columns: []source.Column{{Name: "owner"}, {Name: "state"}}}
	req := planScan(q.Stmt, q.Stmt.From[0], ts, nil)
	assert.Len(t, req.Filters, 2)
}
