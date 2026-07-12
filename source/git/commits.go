package git

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/dmashuda/dfetch/source"
)

// logFormat renders one commit as 10 unit-separated fields; records are
// NUL-separated by -z. body (%b) is last so a pathological separator inside a
// commit message lands there instead of shifting fields.
const logFormat = "%H%x1f%an%x1f%ae%x1f%at%x1f%cn%x1f%ce%x1f%ct%x1f%P%x1f%s%x1f%b"

// logPlan is the pushed-down shape of one commits scan.
type logPlan struct {
	args        []string // git argv, starting with "log"
	ref         string   // value stored in the ref column
	ancestorSha string   // non-empty: verify sha is an ancestor of ref first
	pushedLimit bool     // -n carries the user's LIMIT (+OFFSET)
	capped      bool     // -n is the max_rows safety cap instead
}

// planLog translates the ScanRequest into git log arguments. Consumed exactly:
// `ref` equality (the start point, default HEAD) and `sha` equality
// (--no-walk). A committer_date range narrows the walk via --since/--until
// widened by one second — a superset, so it never counts as consumed and LIMIT
// cannot ride it. LIMIT (+OFFSET) becomes -n only when every filter was
// consumed and there is no ORDER BY (git's log order is not a strict sort);
// otherwise -n is the max_rows cap.
func planLog(req source.ScanRequest, maxRows int) logPlan {
	p := logPlan{ref: "HEAD"}
	args := []string{"log", "-z", "--pretty=format:" + logFormat}
	consumedAll := true
	sha, refGiven := "", false

	for _, f := range req.Filters {
		switch f.Column {
		case "ref":
			if f.Op == source.OpEq {
				if s, ok := f.Value.(string); ok && s != "" {
					p.ref, refGiven = s, true
					continue
				}
			}
		case "sha":
			if f.Op == source.OpEq {
				if s, ok := f.Value.(string); ok && s != "" {
					sha = s
					continue
				}
			}
		case "committer_date":
			if t, ok := parseTime(f.Value); ok {
				switch f.Op {
				case source.OpGt, source.OpGte:
					args = append(args, "--since="+t.Add(-time.Second).UTC().Format(timeLayout))
				case source.OpLt, source.OpLte:
					args = append(args, "--until="+t.Add(time.Second).UTC().Format(timeLayout))
				}
			}
			// The widened window is a superset; SQLite re-applies the exact
			// bound, so this filter is never consumed.
		}
		consumedAll = false
	}

	n := maxRows
	if req.Limit != nil && consumedAll && len(req.OrderBy) == 0 {
		want := *req.Limit
		if req.Offset != nil {
			want += *req.Offset // fetch limit+offset; SQLite re-applies OFFSET
		}
		if want < n {
			n, p.pushedLimit = want, true
		}
	}
	p.capped = !p.pushedLimit
	args = append(args, "-n", strconv.Itoa(n))

	if sha != "" {
		if refGiven {
			p.ancestorSha = sha
		}
		args = append(args, "--no-walk", sha)
	} else {
		args = append(args, p.ref)
	}
	p.args = args
	return p
}

// parseTime reads a filter value as a timestamp: RFC3339 (the column's own
// format), a zoneless timestamp, or a bare date (both taken as UTC).
func parseTime(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func (c *Connector) scanCommits(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	p := planLog(req, c.maxRows)
	if p.ancestorSha != "" {
		ok, err := c.isAncestor(ctx, p.ancestorSha, p.ref)
		if err != nil {
			return err
		}
		if !ok {
			return nil // the commit is not reachable from that ref: no rows
		}
	}

	out, err := c.output(ctx, p.args...)
	if err != nil {
		return err
	}

	cols := c.Tables()[0].ColumnNames()
	w := &rowWriter{cols: cols, emit: emit, stop: c.maxRows}
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		f := strings.SplitN(string(rec), "\x1f", 10)
		if len(f) != 10 {
			return fmt.Errorf("git log: malformed record (%d fields)", len(f))
		}
		parents, err := json.Marshal(strings.Fields(f[7]))
		if err != nil {
			return err
		}
		row := []any{
			p.ref,                     // ref, as the query wrote it (default HEAD)
			f[0],                      // sha
			f[1], f[2], unixUTC(f[3]), // author name/email/date
			f[4], f[5], unixUTC(f[6]), // committer name/email/date
			f[8],                          // subject
			strings.TrimRight(f[9], "\n"), // body
			string(parents),
		}
		done, err := w.add(row)
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
	if p.capped && w.total >= c.maxRows {
		return emit(source.Warn("git.commits: capped at max_rows=%d; raise the max_rows param or add a LIMIT/narrower filters for the rest", c.maxRows))
	}
	return nil
}

// isAncestor reports whether sha is reachable from ref (exit 0 yes, 1 no).
func (c *Connector) isAncestor(ctx context.Context, sha, ref string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", c.repo, "merge-base", "--is-ancestor", sha, ref) //nolint:gosec // G204: same as output(); argv exec, no shell
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	if msg := bytes.TrimSpace(stderr.Bytes()); len(msg) > 0 {
		return false, fmt.Errorf("git merge-base: %s", msg)
	}
	return false, fmt.Errorf("git merge-base: %w", err)
}
