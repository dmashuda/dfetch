// Package git is a dfetch Connector that serves a local git repository as
// tables — commit history, branches, tags, working-tree status, and tracked
// files — so repo questions become SQL (and join against github/jira sources)
// instead of `git log --format | awk` pipelines. It shells out to the `git`
// binary as an argv (no shell), reading NUL-separated records. It is a builtin
// under the `git` schema rooted at the working directory's repository; a
// config source can point elsewhere via params.repo.
package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	"github.com/dmashuda/dfetch/source"
)

const (
	defaultMaxRows = 100000 // cap on a scan whose LIMIT can't be pushed safely
	batchSize      = 1000   // rows per emitted chunk

	// timeLayout is fixed-width UTC so lexical order matches chronological
	// (same convention as the postgres connector's timestamps).
	timeLayout = "2006-01-02T15:04:05Z"
)

// Connector queries one local git repository.
type Connector struct {
	repo    string
	maxRows int
}

// New builds a git connector. Params: "repo" (path to the repository, default
// ".") and "max_rows" (cap on an un-pushable-LIMIT commit scan; default
// 100000). New(nil) works — a missing repo or git binary errors when a git
// table is queried, not at construction — so it is a builtin under the `git`
// schema.
func New(params map[string]any) (source.Connector, error) {
	c := &Connector{repo: ".", maxRows: defaultMaxRows}
	if r, ok := params["repo"].(string); ok && r != "" {
		c.repo = r
	}
	if n, ok := source.IntParam(params["max_rows"]); ok && n > 0 {
		c.maxRows = n
	}
	return c, nil
}

// Tables returns the fixed table set.
func (c *Connector) Tables() []source.TableSchema {
	return []source.TableSchema{
		{Name: "commits", Columns: []source.Column{
			{Name: "ref", Type: "TEXT"},
			{Name: "sha", Type: "TEXT"},
			{Name: "author_name", Type: "TEXT"},
			{Name: "author_email", Type: "TEXT"},
			{Name: "author_date", Type: "TEXT"},
			{Name: "committer_name", Type: "TEXT"},
			{Name: "committer_email", Type: "TEXT"},
			{Name: "committer_date", Type: "TEXT"},
			{Name: "subject", Type: "TEXT"},
			{Name: "body", Type: "TEXT"},
			{Name: "parents", Type: "TEXT"}, // JSON array of parent shas
		}},
		{Name: "branches", Columns: []source.Column{
			{Name: "name", Type: "TEXT"},
			{Name: "sha", Type: "TEXT"},
			{Name: "upstream", Type: "TEXT"},
			{Name: "is_head", Type: "INTEGER"},
			{Name: "committer_date", Type: "TEXT"},
			{Name: "subject", Type: "TEXT"},
		}},
		{Name: "tags", Columns: []source.Column{
			{Name: "name", Type: "TEXT"},
			{Name: "sha", Type: "TEXT"}, // the commit, dereferenced for annotated tags
			{Name: "created_date", Type: "TEXT"},
			{Name: "subject", Type: "TEXT"},
		}},
		{Name: "status", Columns: []source.Column{
			{Name: "path", Type: "TEXT"},
			{Name: "staged", Type: "TEXT"},   // porcelain X: staged/index state
			{Name: "unstaged", Type: "TEXT"}, // porcelain Y: working-tree state
			{Name: "orig_path", Type: "TEXT"},
		}},
		{Name: "files", Columns: []source.Column{
			{Name: "path", Type: "TEXT"},
			{Name: "mode", Type: "TEXT"},
			{Name: "sha", Type: "TEXT"},
		}},
	}
}

// Scan dispatches to the per-table scan.
func (c *Connector) Scan(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	switch req.Table {
	case "commits":
		return c.scanCommits(ctx, req, emit)
	case "branches":
		return c.scanBranches(ctx, emit)
	case "tags":
		return c.scanTags(ctx, emit)
	case "status":
		return c.scanStatus(ctx, emit)
	case "files":
		return c.scanFiles(ctx, emit)
	default:
		return fmt.Errorf("git: unknown table %q", req.Table)
	}
}

// output runs git with the given args and returns its stdout, folding stderr
// into the error. The argv is executed directly — never through a shell.
func (c *Connector) output(ctx context.Context, args ...string) ([]byte, error) {
	argv := append([]string{"-C", c.repo}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...) //nolint:gosec // G204: running git on the user's own repo/query values is the connector's purpose; argv exec, no shell
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := bytes.TrimSpace(stderr.Bytes())
		if len(msg) > 0 {
			return nil, fmt.Errorf("git %s: %s", args[0], msg)
		}
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return stdout.Bytes(), nil
}

// unixUTC renders a git unix timestamp as fixed-width UTC text.
func unixUTC(s string) any {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	return time.Unix(n, 0).UTC().Format(timeLayout)
}

// rowWriter batches rows into emitted chunks and stops the read at a row cap.
type rowWriter struct {
	cols  []string
	emit  func(*source.Rows) error
	batch [][]any
	total int
	stop  int
}

// add appends one row, emitting a chunk when the batch fills. done reports
// that the row cap was reached and the caller should stop reading.
func (w *rowWriter) add(row []any) (done bool, err error) {
	w.batch = append(w.batch, row)
	w.total++
	if len(w.batch) >= batchSize {
		if err := w.flush(); err != nil {
			return false, err
		}
	}
	return w.total >= w.stop, nil
}

// flush emits the pending batch, if any.
func (w *rowWriter) flush() error {
	if len(w.batch) == 0 {
		return nil
	}
	rows := &source.Rows{Columns: w.cols, Rows: w.batch}
	w.batch = nil
	return w.emit(rows)
}
