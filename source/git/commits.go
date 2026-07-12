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

	"github.com/dmashuda/dfetch/source"
)

// logFormat renders one commit as 10 unit-separated fields; records are
// NUL-separated by -z. body (%b) is last so a pathological separator inside a
// commit message lands there instead of shifting fields.
const logFormat = "%H%x1f%an%x1f%ae%x1f%at%x1f%cn%x1f%ce%x1f%ct%x1f%P%x1f%s%x1f%b"

// logPlan is the pushed-down shape of one commits scan.
type logPlan struct {
	args     []string // git log argv, excluding the trailing revision
	rev      string   // the revision to verify and walk (HEAD, a ref, or a sha)
	noWalk   bool     // rev is a sha to fetch directly (--no-walk), not a walk root
	stampRef any      // value for the synthetic ref column (nil = unknown)
	ancestor bool     // verify rev is reachable from stampRef before emitting
	capped   bool     // -n is the max_rows safety cap, not a user LIMIT
	emitCap  int      // rows to emit (max_rows when capped, else the pushed LIMIT)
}

// planLog translates the ScanRequest into a git log plan. Two columns push
// down, and only as a single equality: `ref` selects the walk root (default
// HEAD) and `sha` fetches one commit directly (--no-walk). The ref column is
// synthetic — every row is stamped with the walked ref — so a non-equality
// predicate on it (IN, !=, an inequality) cannot be answered by one walk, and a
// HEAD walk stamped 'HEAD' would be silently wrong; such a predicate is an
// error rather than a wrong answer (mirrors github's required-filter handling).
// No other column pushes down: committer_date is deliberately NOT translated to
// --since/--until, which prune the *traversal* (stopping at the first
// out-of-window commit) and so can drop matching commits in a non-monotonic
// history — not the superset the engine's verbatim re-filter relies on. LIMIT
// (+OFFSET) becomes -n only when every filter was consumed and there is no
// ORDER BY (git's log order is not a strict sort); otherwise -n is the max_rows
// cap plus one, so the caller can tell a real truncation from a full result.
func planLog(req source.ScanRequest, maxRows int) (logPlan, error) {
	p := logPlan{rev: "HEAD"}
	consumedAll := true
	sha, refGiven := "", false
	var refValue any

	for _, f := range req.Filters {
		switch f.Column {
		case "ref":
			s, ok := f.Value.(string)
			if f.Op != source.OpEq || !ok || s == "" {
				return logPlan{}, fmt.Errorf("git.commits: the ref column supports only a single equality filter (e.g. ref='main'); rewrite the query to select one ref")
			}
			p.rev, refGiven, refValue = s, true, f.Value
		case "sha":
			s, ok := f.Value.(string)
			if f.Op != source.OpEq || !ok || s == "" {
				return logPlan{}, fmt.Errorf("git.commits: the sha column supports only a single equality filter (e.g. sha='abc123')")
			}
			sha = s
		default:
			consumedAll = false // e.g. committer_date: left for SQLite to apply
		}
	}

	p.emitCap, p.capped = maxRows, true
	n := maxRows
	if req.Limit != nil && consumedAll && len(req.OrderBy) == 0 {
		want := *req.Limit
		if req.Offset != nil {
			want += *req.Offset // fetch limit+offset; SQLite re-applies OFFSET
		}
		if want < n {
			n, p.emitCap, p.capped = want, want, false
		}
	}
	if p.capped {
		n++ // fetch one extra so a full result isn't misreported as truncated
	}

	// Stamp the ref column truthfully: the walked ref when we walk, or (for a
	// sha lookup constrained to a ref that passes the ancestry check) that ref;
	// NULL for a bare sha lookup, which has no single ref to claim.
	if sha != "" {
		p.rev, p.noWalk, p.ancestor = sha, true, refGiven
		if refGiven {
			p.stampRef = refValue
		}
	} else {
		p.stampRef = p.rev
	}

	p.args = []string{"log", "-z", "--pretty=format:" + logFormat, "-n", strconv.Itoa(n)}
	if p.noWalk {
		p.args = append(p.args, "--no-walk")
	}
	return p, nil
}

func (c *Connector) scanCommits(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	p, err := planLog(req, c.maxRows)
	if err != nil {
		return err
	}

	// A revision git can't resolve (a bad/unknown sha, or HEAD in a repo with
	// no commits yet) means "no such commit" — zero rows, not a scan error.
	resolved, err := c.revExists(ctx, p.rev)
	if err != nil {
		return err
	}
	if !resolved {
		return nil
	}
	if p.ancestor {
		ref, _ := p.stampRef.(string)
		ok, err := c.isAncestor(ctx, p.rev, ref)
		if err != nil {
			return err
		}
		if !ok {
			return nil // the commit is not reachable from that ref: no rows
		}
	}

	// --end-of-options stops git from parsing a revision that begins with '-'
	// (e.g. ref='--output=…') as an option.
	out, err := c.output(ctx, append(append([]string(nil), p.args...), "--end-of-options", p.rev)...)
	if err != nil {
		return err
	}

	w := &rowWriter{cols: c.tableCols("commits"), emit: emit, cap: p.emitCap}
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
			p.stampRef,                // ref (walked ref, or NULL for a bare sha lookup)
			f[0],                      // sha
			f[1], f[2], unixUTC(f[3]), // author name/email/date
			f[4], f[5], unixUTC(f[6]), // committer name/email/date
			f[8],                          // subject
			strings.TrimRight(f[9], "\n"), // body
			string(parents),
		}
		ok, err := w.add(row)
		if err != nil {
			return err
		}
		if !ok {
			break // hit the emit cap; the extra fetched row is dropped
		}
	}
	if err := w.flush(); err != nil {
		return err
	}
	if p.capped && w.truncated {
		return emit(source.Warn("git.commits: capped at max_rows=%d; raise the max_rows param or add a LIMIT/narrower filters for the rest", c.maxRows))
	}
	return nil
}

// revExists reports whether git can resolve rev to a commit. With
// --verify --quiet, an unknown/ill-formed revision exits 1, which the caller
// treats as zero rows rather than an error; any other failure (e.g. exit 128,
// "not a git repository") is a real error and propagates.
func (c *Connector) revExists(ctx context.Context, rev string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", c.repo, //nolint:gosec // G204: same as output(); argv exec, no shell
		"rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil // no such revision
	}
	if msg := bytes.TrimSpace(stderr.Bytes()); len(msg) > 0 {
		return false, fmt.Errorf("git rev-parse: %s", msg)
	}
	return false, fmt.Errorf("git rev-parse: %w", err)
}

// isAncestor reports whether rev is reachable from ref (exit 0 yes, 1 no).
func (c *Connector) isAncestor(ctx context.Context, rev, ref string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", c.repo, //nolint:gosec // G204: same as output(); argv exec, no shell
		"merge-base", "--is-ancestor", "--end-of-options", rev, ref)
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
