package git

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/dmashuda/dfetch/source"
)

// The refs/status/files tables have no push-down: the datasets are small (or
// capped by the shared rowWriter), and SQLite applies the query.

func (c *Connector) scanBranches(ctx context.Context, emit func(*source.Rows) error) error {
	out, err := c.output(ctx, "for-each-ref", "refs/heads",
		"--format=%(refname:short)%1f%(objectname)%1f%(upstream:short)%1f%(HEAD)%1f%(committerdate:unix)%1f%(subject)")
	if err != nil {
		return err
	}
	w := &rowWriter{cols: tableCols(c, "branches"), emit: emit, stop: c.maxRows}
	for line := range strings.Lines(string(out)) {
		f := strings.SplitN(strings.TrimRight(line, "\n"), "\x1f", 6)
		if len(f) != 6 {
			continue
		}
		var upstream any
		if f[2] != "" {
			upstream = f[2]
		}
		done, err := w.add([]any{f[0], f[1], upstream, f[3] == "*", unixUTC(f[4]), f[5]})
		if err != nil {
			return err
		}
		if done {
			break
		}
	}
	return w.flush()
}

func (c *Connector) scanTags(ctx context.Context, emit func(*source.Rows) error) error {
	out, err := c.output(ctx, "for-each-ref", "refs/tags",
		"--format=%(refname:short)%1f%(objectname)%1f%(*objectname)%1f%(creatordate:unix)%1f%(subject)")
	if err != nil {
		return err
	}
	w := &rowWriter{cols: tableCols(c, "tags"), emit: emit, stop: c.maxRows}
	for line := range strings.Lines(string(out)) {
		f := strings.SplitN(strings.TrimRight(line, "\n"), "\x1f", 5)
		if len(f) != 5 {
			continue
		}
		sha := f[1]
		if f[2] != "" { // annotated tag: use the dereferenced commit
			sha = f[2]
		}
		done, err := w.add([]any{f[0], sha, unixUTC(f[3]), f[4]})
		if err != nil {
			return err
		}
		if done {
			break
		}
	}
	return w.flush()
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
	w := &rowWriter{cols: tableCols(c, "status"), emit: emit, stop: c.maxRows}
	for _, row := range rows {
		done, err := w.add(row)
		if err != nil {
			return err
		}
		if done {
			break
		}
	}
	return w.flush()
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
	w := &rowWriter{cols: tableCols(c, "files"), emit: emit, stop: c.maxRows}
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
		done, err := w.add([]any{path, fields[0], fields[1]})
		if err != nil {
			return err
		}
		if done {
			break
		}
	}
	if err := w.flush(); err != nil {
		return err
	}
	if w.total >= c.maxRows {
		return emit(source.Warn("git.files: capped at max_rows=%d; raise the max_rows param for the rest", c.maxRows))
	}
	return nil
}

// tableCols returns the declared column names of one table.
func tableCols(c *Connector, name string) []string {
	for _, ts := range c.Tables() {
		if ts.Name == name {
			return ts.ColumnNames()
		}
	}
	return nil
}
