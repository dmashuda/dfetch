package engine

import (
	"testing"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/sqlparse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func planFor(t *testing.T, sql string) source.ScanRequest {
	t.Helper()
	q, err := sqlparse.Parse(sql)
	require.NoError(t, err)
	require.NotEmpty(t, q.Stmt.From)
	src := q.Stmt.From[0]
	ts := source.TableSchema{Name: src.Name, Columns: []source.Column{
		{Name: "owner"}, {Name: "repo"}, {Name: "state"}, {Name: "updated_at"}, {Name: "comments"},
	}}
	return planScan(q.Stmt, src, ts)
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

func TestPlanScanSkipsBindAndUnknownColumns(t *testing.T) {
	req := planFor(t, "SELECT * FROM github.issues WHERE owner = :o AND missing_col = 'x'")
	// :o is a bind (not pushable); missing_col is not in the table schema.
	assert.Empty(t, req.Filters)
}

func TestPlanScanLimitNotPushedForMultiSource(t *testing.T) {
	q, err := sqlparse.Parse(
		"SELECT * FROM github.issues i JOIN github.repos r ON i.repo = r.name WHERE i.owner = 'golang' LIMIT 5")
	require.NoError(t, err)

	ts := source.TableSchema{Name: "issues", Columns: []source.Column{{Name: "owner"}, {Name: "repo"}}}
	req := planScan(q.Stmt, q.Stmt.From[0], ts)
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

	req := planScan(q.Stmt, reposSrc, reposTS)
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
	req := planScan(q.Stmt, q.Stmt.From[1], reposTS)
	// Only one filter on name, even though it's both direct and inferable.
	assert.Len(t, req.Filters, 1)
}

func TestPlanScanQualifiedColumnAttribution(t *testing.T) {
	q, err := sqlparse.Parse("SELECT * FROM github.issues i WHERE i.owner = 'golang' AND i.state = 'open'")
	require.NoError(t, err)
	ts := source.TableSchema{Name: "issues", Columns: []source.Column{{Name: "owner"}, {Name: "state"}}}
	req := planScan(q.Stmt, q.Stmt.From[0], ts)
	assert.Len(t, req.Filters, 2)
}
