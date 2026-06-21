package ckan

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/dmashuda/dfetch/internal/source"
)

func col(name, typ string) source.Column { return source.Column{Name: name, Type: typ} }

// Column names mirror the CKAN package dict fields so queries read like the API
// docs. "q" is a virtual full-text search column: WHERE q = '...' becomes the
// package_search "q" parameter. It is not a returned field, so the value the
// caller searched for is echoed back into the column (NULL when absent) to keep
// SQLite's verbatim re-filter from dropping the row. Only equality is supported
// (see searchTerm): any other operator on q is rejected, since the column has no
// stored value for SQLite to compare against.
var datasetsCols = []source.Column{
	col("id", "TEXT"), col("name", "TEXT"), col("title", "TEXT"),
	col("notes", "TEXT"), col("author", "TEXT"), col("maintainer", "TEXT"),
	col("license_id", "TEXT"), col("license_title", "TEXT"), col("state", "TEXT"),
	col("private", "INTEGER"), col("organization", "TEXT"), col("organization_title", "TEXT"),
	col("num_resources", "INTEGER"), col("num_tags", "INTEGER"),
	col("metadata_created", "TEXT"), col("metadata_modified", "TEXT"),
	col("url", "TEXT"), col("tags", "TEXT"), col("q", "TEXT"),
}

// resources flattens each dataset's resources[] (the downloadable files). It
// shares the "q" virtual full-text column with datasets; package_id/package_name
// link a resource back to its datagov.datasets row.
var resourcesCols = []source.Column{
	col("id", "TEXT"), col("package_id", "TEXT"), col("package_name", "TEXT"),
	col("name", "TEXT"), col("description", "TEXT"), col("format", "TEXT"),
	col("mimetype", "TEXT"), col("url", "TEXT"), col("size", "INTEGER"),
	col("state", "TEXT"), col("created", "TEXT"), col("last_modified", "TEXT"),
	col("datastore_active", "INTEGER"), col("q", "TEXT"),
}

// orgGroupCols is shared by the organizations and groups tables; both
// organization_list and group_list return the same dict shape with all_fields.
var orgGroupCols = []source.Column{
	col("id", "TEXT"), col("name", "TEXT"), col("title", "TEXT"),
	col("description", "TEXT"), col("package_count", "INTEGER"), col("state", "TEXT"),
	col("type", "TEXT"), col("image_url", "TEXT"), col("created", "TEXT"),
}

func colNames(cols []source.Column) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}

// --- package_search response shapes ---

type pkgSearchResp struct {
	Success bool `json:"success"`
	Result  struct {
		Count   int           `json:"count"`
		Results []ckanPackage `json:"results"`
	} `json:"result"`
}

type ckanPackage struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Title            string         `json:"title"`
	Notes            string         `json:"notes"`
	Author           string         `json:"author"`
	Maintainer       string         `json:"maintainer"`
	LicenseID        string         `json:"license_id"`
	LicenseTitle     string         `json:"license_title"`
	State            string         `json:"state"`
	Private          bool           `json:"private"`
	NumResources     int64          `json:"num_resources"`
	NumTags          int64          `json:"num_tags"`
	MetadataCreated  string         `json:"metadata_created"`
	MetadataModified string         `json:"metadata_modified"`
	URL              string         `json:"url"`
	Organization     *ckanOrg       `json:"organization"`
	Tags             []ckanTag      `json:"tags"`
	Resources        []ckanResource `json:"resources"`
}

type ckanResource struct {
	ID              string          `json:"id"`
	PackageID       string          `json:"package_id"`
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	Format          string          `json:"format"`
	Mimetype        string          `json:"mimetype"`
	URL             string          `json:"url"`
	Size            json.RawMessage `json:"size"` // may be a number, a string, or null
	State           string          `json:"state"`
	Created         string          `json:"created"`
	LastModified    string          `json:"last_modified"`
	DatastoreActive *bool           `json:"datastore_active"`
}

// orgGroupListResp is the envelope for organization_list / group_list with
// all_fields=true: a flat list of org/group dicts.
type orgGroupListResp struct {
	Success bool `json:"success"`
	Result  []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		PackageCount int64  `json:"package_count"`
		State        string `json:"state"`
		Type         string `json:"type"`
		ImageURL     string `json:"image_url"`
		Created      string `json:"created"`
	} `json:"result"`
}

type ckanOrg struct {
	Name  string `json:"name"`
	Title string `json:"title"`
}

type ckanTag struct {
	Name string `json:"name"`
}

func orgName(o *ckanOrg) any {
	if o == nil {
		return nil
	}
	return o.Name
}

func orgTitle(o *ckanOrg) any {
	if o == nil {
		return nil
	}
	return o.Title
}

func tagNames(tags []ckanTag) string {
	names := make([]string, len(tags))
	for i, t := range tags {
		names[i] = t.Name
	}
	return strings.Join(names, ",")
}

// --- datasets ---

func (c *Connector) scanDatasets(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	searchQ, err := searchTerm(req)
	if err != nil {
		return err
	}
	q := url.Values{}
	if s, ok := searchQ.(string); ok {
		q.Set("q", s)
	}
	sortOK, consumed := buildDatasetParams(req, q)
	rows, stopAt, pushLimit := pageLimit(req, sortOK && consumed)

	cols := colNames(datasetsCols)

	sent := 0
	for start, pages := 0, 0; ; start += rows {
		if !pushLimit && pages >= maxPages {
			break
		}
		q.Set("rows", strconv.Itoa(rows))
		q.Set("start", strconv.Itoa(start))

		var resp pkgSearchResp
		if err := c.getJSON(ctx, c.actionURL("package_search", q), &resp); err != nil {
			return err
		}
		if !resp.Success {
			return fmt.Errorf("ckan: package_search returned success=false")
		}
		results := resp.Result.Results
		if len(results) == 0 {
			break
		}

		page := make([][]any, 0, len(results))
		for _, p := range results {
			page = append(page, []any{
				p.ID, p.Name, p.Title, p.Notes, p.Author, p.Maintainer,
				p.LicenseID, p.LicenseTitle, p.State, p.Private,
				orgName(p.Organization), orgTitle(p.Organization),
				p.NumResources, p.NumTags, p.MetadataCreated, p.MetadataModified,
				p.URL, tagNames(p.Tags), searchQ,
			})
			sent++
			if pushLimit && sent >= stopAt {
				break
			}
		}
		if len(page) > 0 {
			if err := emit(&source.Rows{Columns: cols, Rows: page}); err != nil {
				return err
			}
		}
		if pushLimit && sent >= stopAt {
			return nil
		}
		if start+len(results) >= resp.Result.Count {
			break
		}
		pages++
	}
	return nil
}

// buildDatasetParams translates the pushable filters and ORDER BY of req into
// package_search query parameters on q, and reports whether the ORDER BY was
// fully honored and whether every filter was consumed by the API. The two
// together decide whether LIMIT is safe to push: an unconsumed filter or
// unhonored order means the API result is not the exact filtered/ordered set.
// The full-text "q" filter is handled separately by searchTerm (and is already
// set on q by the caller), so here it just counts as consumed.
func buildDatasetParams(req source.ScanRequest, q url.Values) (sortOK, consumed bool) {
	consumed = true
	var fq []string
	for _, f := range req.Filters {
		switch f.Column {
		case "q":
			continue // applied by searchTerm; an equality q is consumed
		case "organization", "license_id", "state", "name", "tags":
			if s, ok := eqString(f); ok {
				fq = append(fq, f.Column+":"+solrQuote(s))
				continue
			}
			if vals, ok := inStrings(f); ok {
				fq = append(fq, f.Column+":("+strings.Join(vals, " OR ")+")")
				continue
			}
		case "metadata_created", "metadata_modified":
			if r, ok := solrRange(f); ok {
				fq = append(fq, f.Column+":"+r)
				continue
			}
		}
		consumed = false
	}
	if len(fq) > 0 {
		q.Set("fq", strings.Join(fq, " AND "))
	}
	sort, sortOK := sortParam(req.OrderBy)
	if sort != "" {
		q.Set("sort", sort)
	}
	return sortOK, consumed
}

// --- resources ---

// scanResources flattens the resources[] of datasets returned by package_search.
// Only the full-text "q" column is pushed down: a dataset-level fq (e.g. on a
// "name") would filter datasets, not resources, and could drop wanted rows — so
// every other predicate is left for SQLite. LIMIT is never pushed (one dataset
// yields many resources, so a dataset-page LIMIT is not a resource LIMIT); the
// scan is bounded by maxPages instead.
func (c *Connector) scanResources(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	searchQ, err := searchTerm(req)
	if err != nil {
		return err
	}
	q := url.Values{}
	if s, ok := searchQ.(string); ok {
		q.Set("q", s)
	}
	cols := colNames(resourcesCols)

	for start, pages := 0, 0; pages < maxPages; start, pages = start+defaultRows, pages+1 {
		q.Set("rows", strconv.Itoa(defaultRows))
		q.Set("start", strconv.Itoa(start))

		var resp pkgSearchResp
		if err := c.getJSON(ctx, c.actionURL("package_search", q), &resp); err != nil {
			return err
		}
		if !resp.Success {
			return fmt.Errorf("ckan: package_search returned success=false")
		}
		results := resp.Result.Results
		if len(results) == 0 {
			break
		}

		var page [][]any
		for _, p := range results {
			for _, r := range p.Resources {
				page = append(page, []any{
					r.ID, r.PackageID, p.Name, r.Name, r.Description, r.Format,
					r.Mimetype, r.URL, sizeVal(r.Size), r.State, r.Created,
					nullableStr(r.LastModified), boolBit(r.DatastoreActive), searchQ,
				})
			}
		}
		if len(page) > 0 {
			if err := emit(&source.Rows{Columns: cols, Rows: page}); err != nil {
				return err
			}
		}
		if start+len(results) >= resp.Result.Count {
			break
		}
	}
	return nil
}

// sizeVal coerces a CKAN resource size (a JSON number, numeric string, or null)
// into an int64 for the INTEGER column, or nil when it is absent/unparseable.
func sizeVal(raw json.RawMessage) any {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	s = strings.Trim(s, `"`) // a "12345" string size
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(f)
	}
	return nil
}

func boolBit(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --- organizations / groups ---

// scanList fetches a flat all_fields list endpoint (organization_list /
// group_list) and emits it as one chunk. These are small reference tables with
// no required filter; SQLite re-applies any WHERE/ORDER BY/LIMIT.
func (c *Connector) scanList(ctx context.Context, action string, cols []source.Column, emit func(*source.Rows) error) error {
	q := url.Values{}
	q.Set("all_fields", "true")

	var resp orgGroupListResp
	if err := c.getJSON(ctx, c.actionURL(action, q), &resp); err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ckan: %s returned success=false", action)
	}
	if len(resp.Result) == 0 {
		return nil
	}

	page := make([][]any, 0, len(resp.Result))
	for _, o := range resp.Result {
		page = append(page, []any{
			o.ID, o.Name, o.Title, o.Description, o.PackageCount,
			o.State, o.Type, o.ImageURL, nullableStr(o.Created),
		})
	}
	return emit(&source.Rows{Columns: colNames(cols), Rows: page})
}
