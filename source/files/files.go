// Package files is a dfetch Connector that serves local data files — CSV, TSV,
// JSON, and JSONL — as tables, so ad-hoc data on disk can be trimmed, joined,
// and aggregated with SQL instead of one-off scripts. It is a dynamic source:
// the table name is the file's path relative to the connector's root, quoted in
// SQL (`SELECT * FROM files."data/orders.csv"`); the schema is inferred from
// the file (CSV/TSV header plus a type-sniffing sample of rows; JSON/JSONL the
// key union of sampled objects); and ListTables walks the root for supported
// files. It is a builtin under the `files` schema rooted at the working
// directory; a config source can re-root it via params.root. Table paths are
// confined to the root by lexical analysis (filepath.IsLocal) — absolute paths
// and ".." escapes are rejected.
package files

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dmashuda/dfetch/source"
)

const (
	defaultMaxRows = 1000000 // cap on a scan whose LIMIT can't be pushed safely
	batchSize      = 1000    // rows per emitted chunk
	sampleRows     = 1000    // rows/objects examined to infer the schema
)

// Connector serves data files under one root directory.
type Connector struct {
	root    string
	maxRows int

	// cache memoizes inferred schemas so a single query doesn't parse the file
	// header twice (the engine's DescribeTable at planning time and Scan's own
	// lookup). One connector lives for one process; files changing mid-run get
	// the schema inferred at first use.
	mu    sync.Mutex
	cache map[string]source.TableSchema
}

// New builds a files connector. Params: "root" (directory the table paths are
// relative to; default ".") and "max_rows" (cap on an un-pushable-LIMIT scan;
// default 1000000). New(nil) works, so it is a builtin under the `files`
// schema.
func New(params map[string]any) (source.Connector, error) {
	c := &Connector{root: ".", maxRows: defaultMaxRows, cache: map[string]source.TableSchema{}}
	if r, ok := params["root"].(string); ok && r != "" {
		c.root = r
	}
	if n, ok := source.IntParam(params["max_rows"]); ok && n > 0 {
		c.maxRows = n
	}
	return c, nil
}

// format is a supported file format, keyed by extension.
type format int

const (
	formatCSV format = iota
	formatTSV
	formatJSON
	formatJSONL
)

// formatOf maps a table name (or file base name) to its format by extension.
func formatOf(name string) (format, bool) {
	switch strings.ToLower(path.Ext(name)) {
	case ".csv":
		return formatCSV, true
	case ".tsv":
		return formatTSV, true
	case ".json":
		return formatJSON, true
	case ".jsonl", ".ndjson":
		return formatJSONL, true
	}
	return 0, false
}

// Tables returns nil: tables are discovered on demand (ListTables/DescribeTable).
func (c *Connector) Tables() []source.TableSchema { return nil }

// resolve maps a table name — a slash-separated path relative to the root — to
// a filesystem path, rejecting anything that escapes the root.
func (c *Connector) resolve(table string) (string, error) {
	p := filepath.Clean(filepath.FromSlash(table))
	if !filepath.IsLocal(p) {
		return "", fmt.Errorf("files: table %q must be a relative path inside %q (no absolute paths or ..)", table, c.root)
	}
	return filepath.Join(c.root, p), nil
}

// maxListEntries bounds an unfiltered walk (opts.Limit == 0), so discovery —
// `dfetch tables` walks the whole working-directory tree since the builtin is
// rooted at "." — can't wander an enormous tree unbounded. A query never
// depends on this: it resolves its file by path via DescribeTable/Scan.
const maxListEntries = 10000

// ListTables walks the root for supported data files and returns their
// slash-separated relative paths. Hidden files and directories (dot-prefixed)
// are skipped, an unreadable directory is skipped rather than failing the whole
// listing, and an unfiltered walk stops at maxListEntries.
func (c *Connector) ListTables(ctx context.Context, opts source.ListOptions) ([]string, error) {
	limit := opts.Limit
	if limit <= 0 || limit > maxListEntries {
		limit = maxListEntries
	}
	var names []string
	walkErr := filepath.WalkDir(c.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory (permission denied) shouldn't sink the
			// whole listing; skip it and keep walking the readable rest.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			if p != c.root && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if _, ok := formatOf(d.Name()); !ok {
			return nil
		}
		rel, err := filepath.Rel(c.root, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if opts.Filter != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(opts.Filter)) {
			return nil
		}
		names = append(names, name)
		if len(names) >= limit {
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("files: listing %q: %w", c.root, walkErr)
	}
	return names, nil
}

// DescribeTable infers one file's schema. found is false (nil error) when the
// path has an unsupported extension or the file does not exist; a supported
// file that exists but cannot be parsed is an error.
func (c *Connector) DescribeTable(ctx context.Context, table string) (source.TableSchema, bool, error) {
	fm, supported := formatOf(table)
	if !supported {
		// Distinguish a directory and an unsupported existing file (helpful
		// errors) from "no such file" (the engine's usual unknown-table error).
		if p, rerr := c.resolve(table); rerr == nil {
			if info, serr := os.Stat(p); serr == nil {
				if info.IsDir() {
					return source.TableSchema{}, false, fmt.Errorf("files: %s is a directory; query the data files inside it (e.g. %s/<file>.csv)", table, table)
				}
				return source.TableSchema{}, false, fmt.Errorf("files: %s: unsupported file type (supported: .csv, .tsv, .json, .jsonl, .ndjson)", table)
			}
		}
		return source.TableSchema{}, false, nil
	}

	c.mu.Lock()
	ts, cached := c.cache[table]
	c.mu.Unlock()
	if cached {
		return ts, true, nil
	}

	p, err := c.resolve(table)
	if err != nil {
		return source.TableSchema{}, false, err
	}
	file, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return source.TableSchema{}, false, nil
	}
	if err != nil {
		return source.TableSchema{}, false, fmt.Errorf("files: opening %s: %w", table, err)
	}
	defer func() { _ = file.Close() }()

	var cols []source.Column
	switch fm {
	case formatCSV:
		cols, err = describeCSV(file, ',')
	case formatTSV:
		cols, err = describeCSV(file, '\t')
	case formatJSON:
		cols, err = describeJSON(file, false)
	case formatJSONL:
		cols, err = describeJSON(file, true)
	}
	if err != nil {
		return source.TableSchema{}, false, fmt.Errorf("files: inferring schema of %s: %w", table, err)
	}

	ts = source.TableSchema{Name: table, Columns: cols}
	c.mu.Lock()
	c.cache[table] = ts
	c.mu.Unlock()
	return ts, true, nil
}

// Scan reads the file and emits rows in batches. A file yields its rows in
// file order with no filtering, so LIMIT/OFFSET are honored (reading stops
// early) only when the query pushed no filters and no ordering; otherwise the
// whole file is read, capped at max_rows with a warning.
func (c *Connector) Scan(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	ts, found, err := c.DescribeTable(ctx, req.Table)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("files: no file %q under %q", req.Table, c.root)
	}

	p, err := c.resolve(req.Table)
	if err != nil {
		return err
	}
	file, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("files: opening %s: %w", req.Table, err)
	}
	defer func() { _ = file.Close() }()

	rowCap, pushed := c.maxRows, false
	if req.Limit != nil && len(req.Filters) == 0 && len(req.OrderBy) == 0 {
		n := *req.Limit
		if req.Offset != nil {
			n += *req.Offset // fetch limit+offset; SQLite re-applies OFFSET verbatim
		}
		if n < rowCap {
			rowCap, pushed = n, true
		}
	}

	w := &rowWriter{cols: ts.ColumnNames(), emit: emit, cap: rowCap}
	fm, _ := formatOf(req.Table)
	switch fm {
	case formatCSV:
		err = scanCSV(file, ',', ts.Columns, w)
	case formatTSV:
		err = scanCSV(file, '\t', ts.Columns, w)
	case formatJSON:
		err = scanJSON(file, false, ts.Columns, w)
	case formatJSONL:
		err = scanJSON(file, true, ts.Columns, w)
	}
	if err != nil {
		return fmt.Errorf("files: reading %s: %w", req.Table, err)
	}
	if err := w.flush(); err != nil {
		return err
	}
	// Warn only when the max_rows safety cap (not a pushed user LIMIT) actually
	// truncated the file — w.truncated is set only when a row past the cap was
	// offered, so an exactly-max_rows file does not warn.
	if !pushed && w.truncated {
		return emit(source.Warn("files.%s: capped at max_rows=%d; raise the max_rows param or add a LIMIT for the rest", req.Table, c.maxRows))
	}
	if len(w.dropped) > 0 {
		return emit(source.Warn("files.%s: %d column(s) not in the inferred schema were dropped (%s); they appear beyond the first %d sampled rows",
			req.Table, len(w.dropped), strings.Join(w.dropped, ", "), sampleRows))
	}
	return nil
}

// rowWriter batches rows into emitted chunks and enforces a row cap. Callers
// add every source row and stop when add reports the cap was hit; a row offered
// past the cap sets truncated, so an exactly-cap result is not misreported as
// truncated.
type rowWriter struct {
	cols      []string
	emit      func(*source.Rows) error
	batch     [][]any
	total     int
	cap       int // max rows to emit; 0 = unlimited
	truncated bool
	dropped   []string // keys seen in data but absent from the inferred schema
}

// add appends one row, emitting a chunk when the batch fills. It returns
// ok=false once the cap is reached (the row is dropped and truncated set), so
// the caller stops reading.
func (w *rowWriter) add(row []any) (ok bool, err error) {
	if w.cap > 0 && w.total >= w.cap {
		w.truncated = true
		return false, nil
	}
	w.batch = append(w.batch, row)
	w.total++
	if len(w.batch) >= batchSize {
		if err := w.flush(); err != nil {
			return false, err
		}
	}
	return true, nil
}

// noteDropped records a data key that has no column in the inferred schema
// (deduped), so Scan can warn that the schema is incomplete. The list is capped
// so a wildly heterogeneous file can't accumulate unboundedly.
func (w *rowWriter) noteDropped(key string) {
	if len(w.dropped) >= 32 {
		return
	}
	for _, k := range w.dropped {
		if k == key {
			return
		}
	}
	w.dropped = append(w.dropped, key)
}

// flush emits the pending batch, if any.
func (w *rowWriter) flush() error {
	if len(w.batch) == 0 {
		return nil
	}
	rows := &source.Rows{Columns: w.cols, Rows: w.batch}
	w.batch = make([][]any, 0, batchSize) // fresh: emit handed off the old backing array
	return w.emit(rows)
}

// stripBOM removes a leading UTF-8 byte-order mark, which spreadsheet exports
// commonly prepend and encoding/csv and encoding/json do not tolerate.
func stripBOM(r io.Reader) io.Reader {
	br := bufio.NewReader(r)
	if b, err := br.Peek(3); err == nil && bytes.Equal(b, []byte{0xEF, 0xBB, 0xBF}) {
		_, _ = br.Discard(3)
	}
	return br
}
