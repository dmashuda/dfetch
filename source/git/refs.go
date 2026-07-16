package git

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/dmashuda/dfetch/source"
)

// The refs/status/files tables have no filter push-down: the datasets are small
// (or capped by the shared rowWriter), and SQLite applies the query. Each caps
// at max_rows and warns via capWarn when the cap actually truncates.

func (c *Connector) scanBranches(ctx context.Context, emit func(*source.Rows) error) error {
	out, err := c.output(ctx, "for-each-ref", "refs/heads",
		"--format=%(refname:short)%1f%(objectname)%1f%(upstream:short)%1f%(HEAD)%1f%(committerdate:unix)%1f%(subject)")
	if err != nil {
		return err
	}
	w := &rowWriter{cols: c.tableCols("branches"), emit: emit, cap: c.maxRows}
	for line := range strings.Lines(string(out)) {
		f := strings.SplitN(strings.TrimRight(line, "\n"), "\x1f", 6)
		if len(f) != 6 {
			continue
		}
		var upstream any
		if f[2] != "" {
			upstream = f[2]
		}
		ok, err := w.add([]any{f[0], f[1], upstream, f[3] == "*", unixUTC(f[4]), f[5]})
		if err != nil {
			return err
		}
		if !ok {
			break
		}
	}
	return c.finish(w, emit, "branches")
}

func (c *Connector) scanTags(ctx context.Context, emit func(*source.Rows) error) error {
	out, err := c.output(ctx, "for-each-ref", "refs/tags",
		"--format=%(refname:short)%1f%(objectname)%1f%(*objectname)%1f%(creatordate:unix)%1f%(subject)")
	if err != nil {
		return err
	}
	w := &rowWriter{cols: c.tableCols("tags"), emit: emit, cap: c.maxRows}
	for line := range strings.Lines(string(out)) {
		f := strings.SplitN(strings.TrimRight(line, "\n"), "\x1f", 5)
		if len(f) != 5 {
			continue
		}
		sha := f[1]
		if f[2] != "" { // annotated tag: use the dereferenced commit
			sha = f[2]
		}
		ok, err := w.add([]any{f[0], sha, unixUTC(f[3]), f[4]})
		if err != nil {
			return err
		}
		if !ok {
			break
		}
	}
	return c.finish(w, emit, "tags")
}

func (c *Connector) scanStatus(ctx context.Context, emit func(*source.Rows) error) error {
	out, err := c.output(ctx, "status", "--porcelain=v1", "-z")
	if err != nil {
		return err
	}
	rows, err := parseStatusZ(out)
	if err != nil {
		return err
	}
	w := &rowWriter{cols: c.tableCols("status"), emit: emit, cap: c.maxRows}
	for _, row := range rows {
		ok, err := w.add(row)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
	}
	return c.finish(w, emit, "status")
}

// parseStatusZ parses `git status --porcelain=v1 -z`: entries are
// "XY PATH" NUL, with a rename/copy followed by the original path and NUL.
func parseStatusZ(out []byte) ([][]any, error) {
	var rows [][]any
	for len(out) > 0 {
		nul := bytes.IndexByte(out, 0)
		if nul < 0 {
			nul = len(out)
		}
		entry := out[:nul]
		out = out[min(nul+1, len(out)):]
		if len(entry) < 4 || entry[2] != ' ' {
			return nil, fmt.Errorf("git status: malformed entry %q", entry)
		}
		staged, unstaged, path := string(entry[0]), string(entry[1]), string(entry[3:])
		var origPath any
		if staged == "R" || staged == "C" || unstaged == "R" || unstaged == "C" {
			nul = bytes.IndexByte(out, 0)
			if nul < 0 {
				nul = len(out)
			}
			origPath = string(out[:nul])
			out = out[min(nul+1, len(out)):]
		}
		rows = append(rows, []any{path, staged, unstaged, origPath})
	}
	return rows, nil
}

func (c *Connector) scanFiles(ctx context.Context, emit func(*source.Rows) error) error {
	out, err := c.output(ctx, "ls-files", "-s", "-z")
	if err != nil {
		return err
	}
	w := &rowWriter{cols: c.tableCols("files"), emit: emit, cap: c.maxRows}
	for _, entry := range bytes.Split(out, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		// "MODE SHA STAGE\tPATH"
		meta, path, ok := strings.Cut(string(entry), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return fmt.Errorf("git ls-files: malformed entry %q", entry)
		}
		added, err := w.add([]any{path, fields[0], fields[1]})
		if err != nil {
			return err
		}
		if !added {
			break
		}
	}
	return c.finish(w, emit, "files")
}

// finish flushes the writer and, when the row cap truncated the result, emits a
// warning naming the table and max_rows.
func (c *Connector) finish(w *rowWriter, emit func(*source.Rows) error, table string) error {
	if err := w.flush(); err != nil {
		return err
	}
	if w.truncated {
		return emit(source.Warn("git.%s: capped at max_rows=%d; raise the max_rows param for the rest", table, c.maxRows))
	}
	return nil
}

// tableCols returns the declared column names of one table.
func (c *Connector) tableCols(name string) []string {
	for _, ts := range c.Tables() {
		if ts.Name == name {
			return ts.ColumnNames()
		}
	}
	return nil
}
