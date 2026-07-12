package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dmashuda/dfetch/engine"
	"github.com/dmashuda/dfetch/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	date1 = "2026-01-01T10:00:00Z"
	date2 = "2026-01-02T10:00:00Z"
	date3 = "2026-01-03T10:00:00Z"
	date4 = "2026-01-04T10:00:00Z"
)

// fixtureRepo builds a deterministic repo: three commits on main (the second
// tagged v1 annotated, the third v2 lightweight), a feature branch with a
// fourth commit, and a dirty working tree (modified file, untracked file,
// staged rename).
func fixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(env []string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: test fixture argv
		cmd.Env = append(os.Environ(),
			append([]string{
				"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
				"GIT_AUTHOR_NAME=Test User", "GIT_AUTHOR_EMAIL=test@example.com",
				"GIT_COMMITTER_NAME=Test User", "GIT_COMMITTER_EMAIL=test@example.com",
			}, env...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	write := func(name, content string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	commit := func(msg, date string) {
		t.Helper()
		run(nil, "add", "-A")
		run([]string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date},
			"commit", "-q", "-m", msg)
	}

	run(nil, "init", "-q", "-b", "main")
	write("a.txt", "one")
	commit("first commit", date1)
	write("b.txt", "two")
	commit("second commit\n\nwith a body", date2)
	run(nil, "tag", "-a", "v1", "-m", "release v1")
	write("a.txt", "one-more")
	commit("third commit", date3)
	run(nil, "tag", "v2")

	run(nil, "checkout", "-q", "-b", "feature")
	write("c.txt", "three")
	commit("feature commit", date4)
	run(nil, "checkout", "-q", "main")

	// Dirty working tree: modified, untracked, staged rename.
	write("a.txt", "changed")
	write("untracked.txt", "new")
	run(nil, "mv", "b.txt", "renamed.txt")
	return dir
}

func newConn(t *testing.T, repo string, params map[string]any) *Connector {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	params["repo"] = repo
	c, err := New(params)
	require.NoError(t, err)
	return c.(*Connector)
}

// collectScan drives Scan and accumulates rows and warnings across chunks.
func collectScan(t *testing.T, c *Connector, req source.ScanRequest) (rows [][]any, cols []string, warnings []string) {
	t.Helper()
	err := c.Scan(context.Background(), req, func(r *source.Rows) error {
		if len(r.Columns) > 0 {
			cols = r.Columns
		}
		rows = append(rows, r.Rows...)
		warnings = append(warnings, r.Warnings...)
		return nil
	})
	require.NoError(t, err)
	return rows, cols, warnings
}

func col(t *testing.T, cols []string, name string) int {
	t.Helper()
	for i, c := range cols {
		if c == name {
			return i
		}
	}
	require.Failf(t, "column not found", "no column %q in %v", name, cols)
	return -1
}

func TestTables(t *testing.T) {
	c, err := New(nil)
	require.NoError(t, err)
	names := make([]string, 0, len(c.Tables()))
	for _, ts := range c.Tables() {
		names = append(names, ts.Name)
		assert.NotEmpty(t, ts.Columns, ts.Name)
	}
	assert.Equal(t, []string{"commits", "branches", "tags", "status", "files"}, names)
	assert.Equal(t, ".", c.(*Connector).repo, "New(nil) defaults to the working directory")
}

// --- planLog (pure push-down logic) ---

func TestPlanLogDefaults(t *testing.T) {
	p, err := planLog(source.ScanRequest{Table: "commits"}, 500)
	require.NoError(t, err)
	assert.Equal(t, "HEAD", p.rev)
	assert.Equal(t, "HEAD", p.stampRef, "default walk stamps ref='HEAD'")
	assert.True(t, p.capped)
	assert.Equal(t, 500, p.emitCap)
	assert.Contains(t, p.args, "501", "capped scans fetch one extra to detect truncation")
}

func TestPlanLogPushesLimitWhenConsumed(t *testing.T) {
	limit, offset := 10, 5
	p, err := planLog(source.ScanRequest{
		Table:   "commits",
		Filters: []source.Filter{{Column: "ref", Op: source.OpEq, Value: "main"}},
		Limit:   &limit,
		Offset:  &offset,
	}, 500)
	require.NoError(t, err)
	assert.Equal(t, "main", p.rev)
	assert.False(t, p.capped)
	assert.Contains(t, p.args, "15", "-n carries limit+offset, no +1 for a pushed LIMIT")
}

func TestPlanLogHoldsLimitBack(t *testing.T) {
	limit := 10
	for name, req := range map[string]source.ScanRequest{
		"order by": {
			Table:   "commits",
			OrderBy: []source.OrderTerm{{Column: "committer_date", Desc: true}},
			Limit:   &limit,
		},
		"unconsumed filter": {
			Table:   "commits",
			Filters: []source.Filter{{Column: "author_email", Op: source.OpEq, Value: "x@y"}},
			Limit:   &limit,
		},
		"date range": {
			Table:   "commits",
			Filters: []source.Filter{{Column: "committer_date", Op: source.OpGt, Value: date1}},
			Limit:   &limit,
		},
	} {
		p, err := planLog(req, 500)
		require.NoError(t, err, name)
		assert.True(t, p.capped, name)
		assert.Contains(t, p.args, "501", name)
	}
}

// committer_date must NOT be pushed as --since/--until: those prune traversal
// and can drop matching commits in a non-monotonic history (not a superset).
func TestPlanLogDoesNotPushDate(t *testing.T) {
	p, err := planLog(source.ScanRequest{
		Table: "commits",
		Filters: []source.Filter{
			{Column: "committer_date", Op: source.OpGte, Value: date2},
			{Column: "committer_date", Op: source.OpLt, Value: date3},
		},
	}, 500)
	require.NoError(t, err)
	for _, a := range p.args {
		assert.NotContains(t, a, "--since")
		assert.NotContains(t, a, "--until")
	}
}

func TestPlanLogSha(t *testing.T) {
	p, err := planLog(source.ScanRequest{
		Table:   "commits",
		Filters: []source.Filter{{Column: "sha", Op: source.OpEq, Value: "abc123"}},
	}, 500)
	require.NoError(t, err)
	assert.Contains(t, p.args, "--no-walk")
	assert.Equal(t, "abc123", p.rev)
	assert.False(t, p.ancestor, "no explicit ref: no ancestry check")
	assert.Nil(t, p.stampRef, "a bare sha lookup stamps ref=NULL, not a fabricated HEAD")

	p, err = planLog(source.ScanRequest{
		Table: "commits",
		Filters: []source.Filter{
			{Column: "sha", Op: source.OpEq, Value: "abc123"},
			{Column: "ref", Op: source.OpEq, Value: "main"},
		},
	}, 500)
	require.NoError(t, err)
	assert.True(t, p.ancestor, "explicit ref: verify ancestry")
	assert.Equal(t, "main", p.stampRef, "sha+ref stamps the ref")
	assert.Equal(t, "abc123", p.rev)
}

// A non-equality predicate on the synthetic ref/sha columns cannot be answered
// by one walk, so planLog errors rather than silently walking HEAD.
func TestPlanLogRejectsNonEquality(t *testing.T) {
	for name, f := range map[string]source.Filter{
		"ref IN":   {Column: "ref", Op: source.OpIn, Values: []any{"main", "dev"}},
		"ref !=":   {Column: "ref", Op: source.OpNotEq, Value: "main"},
		"sha IN":   {Column: "sha", Op: source.OpIn, Values: []any{"a", "b"}},
		"ref LIKE": {Column: "ref", Op: source.OpLike, Value: "ma%"},
	} {
		_, err := planLog(source.ScanRequest{Table: "commits", Filters: []source.Filter{f}}, 500)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "equality", name)
	}
}

// --- behavioral tests against a real repo ---

func TestScanCommits(t *testing.T) {
	c := newConn(t, fixtureRepo(t), nil)
	rows, cols, warnings := collectScan(t, c, source.ScanRequest{Table: "commits"})
	require.Len(t, rows, 3, "HEAD (main) has three commits")
	assert.Empty(t, warnings)

	subject, cdate, ref := col(t, cols, "subject"), col(t, cols, "committer_date"), col(t, cols, "ref")
	assert.Equal(t, "third commit", rows[0][subject])
	assert.Equal(t, "2026-01-03T10:00:00Z", rows[0][cdate], "fixed-width UTC")
	assert.Equal(t, "HEAD", rows[0][ref])

	body, parents := col(t, cols, "body"), col(t, cols, "parents")
	assert.Equal(t, "with a body", rows[1][body])
	assert.Equal(t, "", rows[2][body])
	assert.Equal(t, "[]", rows[2][parents], "root commit has no parents")
	assert.Contains(t, rows[1][parents], rows[2][col(t, cols, "sha")], "parents is a JSON array of shas")
}

func TestScanCommitsRefFilter(t *testing.T) {
	c := newConn(t, fixtureRepo(t), nil)
	rows, cols, _ := collectScan(t, c, source.ScanRequest{
		Table:   "commits",
		Filters: []source.Filter{{Column: "ref", Op: source.OpEq, Value: "feature"}},
	})
	require.Len(t, rows, 4)
	assert.Equal(t, "feature commit", rows[0][col(t, cols, "subject")])
	assert.Equal(t, "feature", rows[0][col(t, cols, "ref")], "ref column stores the value as written")
}

func TestScanCommitsShaFilter(t *testing.T) {
	c := newConn(t, fixtureRepo(t), nil)
	all, cols, _ := collectScan(t, c, source.ScanRequest{Table: "commits"})
	sha := all[1][col(t, cols, "sha")].(string)

	rows, _, _ := collectScan(t, c, source.ScanRequest{
		Table:   "commits",
		Filters: []source.Filter{{Column: "sha", Op: source.OpEq, Value: sha}},
	})
	require.Len(t, rows, 1)
	assert.Equal(t, sha, rows[0][col(t, cols, "sha")])
	assert.Nil(t, rows[0][col(t, cols, "ref")], "a bare sha lookup does not fabricate a ref")
}

// A well-formed but nonexistent sha yields zero rows, not a scan error.
func TestScanCommitsBadSha(t *testing.T) {
	c := newConn(t, fixtureRepo(t), nil)
	rows, _, _ := collectScan(t, c, source.ScanRequest{
		Table:   "commits",
		Filters: []source.Filter{{Column: "sha", Op: source.OpEq, Value: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}},
	})
	assert.Empty(t, rows)
}

// A ref/sha value beginning with '-' is treated as a revision (which doesn't
// exist → zero rows), never parsed as a git option.
func TestScanCommitsDashRefIsNotAFlag(t *testing.T) {
	dir := fixtureRepo(t)
	c := newConn(t, dir, nil)
	rows, _, _ := collectScan(t, c, source.ScanRequest{
		Table:   "commits",
		Filters: []source.Filter{{Column: "ref", Op: source.OpEq, Value: "--output=" + filepath.Join(dir, "pwned")}},
	})
	assert.Empty(t, rows)
	_, err := os.Stat(filepath.Join(dir, "pwned"))
	assert.True(t, os.IsNotExist(err), "git must not have written an --output file")
}

// A non-equality predicate on ref is rejected rather than silently walking HEAD.
func TestScanCommitsRejectsRefIn(t *testing.T) {
	c := newConn(t, fixtureRepo(t), nil)
	err := c.Scan(context.Background(), source.ScanRequest{
		Table:   "commits",
		Filters: []source.Filter{{Column: "ref", Op: source.OpIn, Values: []any{"main", "feature"}}},
	}, func(*source.Rows) error { return nil })
	require.ErrorContains(t, err, "equality")
}

// An empty repository (no commits) yields zero rows for every table, no error.
func TestScanEmptyRepo(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("git", "-C", dir, "init", "-q", "-b", "main") //nolint:gosec // G204: test fixture argv
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	require.NoError(t, cmd.Run())
	c := newConn(t, dir, nil)
	for _, table := range []string{"commits", "branches", "tags", "status", "files"} {
		rows, _, _ := collectScan(t, c, source.ScanRequest{Table: table})
		assert.Empty(t, rows, table)
	}
}

func TestScanCommitsShaNotOnRef(t *testing.T) {
	c := newConn(t, fixtureRepo(t), nil)
	feature, cols, _ := collectScan(t, c, source.ScanRequest{
		Table:   "commits",
		Filters: []source.Filter{{Column: "ref", Op: source.OpEq, Value: "feature"}},
	})
	featureSha := feature[0][col(t, cols, "sha")].(string)

	rows, _, _ := collectScan(t, c, source.ScanRequest{
		Table: "commits",
		Filters: []source.Filter{
			{Column: "sha", Op: source.OpEq, Value: featureSha},
			{Column: "ref", Op: source.OpEq, Value: "main"},
		},
	})
	assert.Empty(t, rows, "the feature commit is not reachable from main")
}

func TestScanCommitsLimit(t *testing.T) {
	c := newConn(t, fixtureRepo(t), nil)
	limit := 2
	rows, _, warnings := collectScan(t, c, source.ScanRequest{Table: "commits", Limit: &limit})
	assert.Len(t, rows, 2)
	assert.Empty(t, warnings)
}

// The connector does NOT push committer_date, so Scan returns the full history
// (a superset); SQLite applies the exact bound. This guards against re-adding
// the traversal-pruning --since/--until push-down.
func TestScanCommitsDateNotPushed(t *testing.T) {
	c := newConn(t, fixtureRepo(t), nil)
	rows, cols, _ := collectScan(t, c, source.ScanRequest{
		Table:   "commits",
		Filters: []source.Filter{{Column: "committer_date", Op: source.OpGt, Value: date2}},
	})
	subjects := make([]string, len(rows))
	for i, r := range rows {
		subjects[i] = r[col(t, cols, "subject")].(string)
	}
	assert.Contains(t, subjects, "first commit", "connector returns a superset; SQLite filters by date")
	assert.Len(t, rows, 3)
}

// End to end: date filtering is correct because SQLite applies it to the
// superset the connector returns.
func TestEngineDateFilter(t *testing.T) {
	c := newConn(t, fixtureRepo(t), nil)
	e, err := engine.New(engine.WithConnector("git", c))
	require.NoError(t, err)
	res, err := e.Run(context.Background(),
		`SELECT subject FROM git.commits WHERE committer_date > '2026-01-02T10:00:00Z' ORDER BY committer_date`)
	require.NoError(t, err)
	require.Len(t, res.Rows, 1)
	assert.Equal(t, "third commit", res.Rows[0][0])
}

func TestScanCommitsMaxRowsWarns(t *testing.T) {
	c := newConn(t, fixtureRepo(t), map[string]any{"max_rows": 2})
	rows, _, warnings := collectScan(t, c, source.ScanRequest{Table: "commits"})
	assert.Len(t, rows, 2)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "max_rows=2")
}

func TestScanBranches(t *testing.T) {
	c := newConn(t, fixtureRepo(t), nil)
	rows, cols, _ := collectScan(t, c, source.ScanRequest{Table: "branches"})
	require.Len(t, rows, 2)
	name, isHead := col(t, cols, "name"), col(t, cols, "is_head")
	byName := map[string][]any{}
	for _, r := range rows {
		byName[r[name].(string)] = r
	}
	assert.Equal(t, true, byName["main"][isHead])
	assert.Equal(t, false, byName["feature"][isHead])
	assert.Equal(t, "2026-01-04T10:00:00Z", byName["feature"][col(t, cols, "committer_date")])
}

// branches/tags/status warn on cap truncation, like commits/files (not silent).
func TestScanBranchesCapWarns(t *testing.T) {
	c := newConn(t, fixtureRepo(t), map[string]any{"max_rows": 1}) // 2 branches, cap 1
	rows, _, warnings := collectScan(t, c, source.ScanRequest{Table: "branches"})
	assert.Len(t, rows, 1)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "git.branches")
	assert.Contains(t, warnings[0], "max_rows=1")

	// Exactly at the cap (2 branches, cap 2): no false truncation warning.
	c = newConn(t, fixtureRepo(t), map[string]any{"max_rows": 2})
	rows, _, warnings = collectScan(t, c, source.ScanRequest{Table: "branches"})
	assert.Len(t, rows, 2)
	assert.Empty(t, warnings, "an exactly-cap result is not truncated")
}

func TestScanTags(t *testing.T) {
	c := newConn(t, fixtureRepo(t), nil)
	commits, ccols, _ := collectScan(t, c, source.ScanRequest{Table: "commits"})
	shaOf := func(subject string) string {
		for _, r := range commits {
			if r[col(t, ccols, "subject")] == subject {
				return r[col(t, ccols, "sha")].(string)
			}
		}
		require.Failf(t, "commit not found", "no commit %q", subject)
		return ""
	}

	rows, cols, _ := collectScan(t, c, source.ScanRequest{Table: "tags"})
	require.Len(t, rows, 2)
	name, sha := col(t, cols, "name"), col(t, cols, "sha")
	byName := map[string][]any{}
	for _, r := range rows {
		byName[r[name].(string)] = r
	}
	assert.Equal(t, shaOf("second commit"), byName["v1"][sha], "annotated tag dereferences to the commit")
	assert.Equal(t, shaOf("third commit"), byName["v2"][sha], "lightweight tag points at the commit")
	assert.Equal(t, "release v1", byName["v1"][col(t, cols, "subject")])
}

func TestScanStatus(t *testing.T) {
	c := newConn(t, fixtureRepo(t), nil)
	rows, cols, _ := collectScan(t, c, source.ScanRequest{Table: "status"})
	path, staged, unstaged, orig := col(t, cols, "path"), col(t, cols, "staged"), col(t, cols, "unstaged"), col(t, cols, "orig_path")
	byPath := map[string][]any{}
	for _, r := range rows {
		byPath[r[path].(string)] = r
	}
	require.Contains(t, byPath, "a.txt")
	assert.Equal(t, "M", byPath["a.txt"][unstaged])
	require.Contains(t, byPath, "untracked.txt")
	assert.Equal(t, "?", byPath["untracked.txt"][staged])
	require.Contains(t, byPath, "renamed.txt")
	assert.Equal(t, "R", byPath["renamed.txt"][staged])
	assert.Equal(t, "b.txt", byPath["renamed.txt"][orig])
}

func TestScanFiles(t *testing.T) {
	c := newConn(t, fixtureRepo(t), nil)
	rows, cols, _ := collectScan(t, c, source.ScanRequest{Table: "files"})
	path, mode := col(t, cols, "path"), col(t, cols, "mode")
	paths := make([]string, 0, len(rows))
	for _, r := range rows {
		paths = append(paths, r[path].(string))
		assert.Equal(t, "100644", r[mode])
	}
	assert.ElementsMatch(t, []string{"a.txt", "renamed.txt"}, paths,
		"index contents on main with the staged rename applied (c.txt is only on feature)")
}

func TestParseStatusZ(t *testing.T) {
	rows, err := parseStatusZ([]byte("R  new.txt\x00old.txt\x00?? x.txt\x00"))
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, []any{"new.txt", "R", " ", "old.txt"}, rows[0])
	assert.Equal(t, []any{"x.txt", "?", "?", nil}, rows[1])

	_, err = parseStatusZ([]byte("bad"))
	require.Error(t, err)
}

func TestScanErrors(t *testing.T) {
	c := newConn(t, t.TempDir(), nil) // not a repository
	err := c.Scan(context.Background(), source.ScanRequest{Table: "commits"}, func(*source.Rows) error { return nil })
	require.Error(t, err, "querying a non-repository is an error, not zero rows")
	assert.Contains(t, err.Error(), "git rev-parse")

	err = c.Scan(context.Background(), source.ScanRequest{Table: "nope"}, func(*source.Rows) error { return nil })
	require.ErrorContains(t, err, "unknown table")
}

// End to end through the engine: parse -> resolve -> load -> SELECT.
func TestEngineEndToEnd(t *testing.T) {
	c := newConn(t, fixtureRepo(t), nil)
	e, err := engine.New(engine.WithConnector("git", c))
	require.NoError(t, err)

	res, err := e.Run(context.Background(),
		`SELECT subject, committer_date FROM git.commits WHERE ref='main' ORDER BY committer_date DESC LIMIT 2`)
	require.NoError(t, err)
	require.Len(t, res.Rows, 2)
	assert.Equal(t, "third commit", res.Rows[0][0])
	assert.Equal(t, "second commit", res.Rows[1][0])
}
