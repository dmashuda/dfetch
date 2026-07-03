package jira

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/sqlparse"
)

// jqlTimeLayout is the JQL date-time literal format ("yyyy-MM-dd HH:mm"); JQL
// datetime comparisons have minute granularity.
const jqlTimeLayout = "2006-01-02 15:04"

// defaultBoundClause is pushed when the query's WHERE has no translatable
// restriction on jira.issues, so a bare `SELECT * FROM jira.issues LIMIT 10`
// still runs a bounded JQL search instead of the (rejected) unbounded one.
const defaultBoundClause = "created >= -30d"

// jqlPlan is the result of translating a jira.issues ScanRequest into JQL.
type jqlPlan struct {
	JQL string
	// ConsumedAll is true when every filter in the request was translated into
	// the JQL (so the API result is the exact filtered set, not a superset) —
	// required before a LIMIT may be pushed. The boundedness default does not
	// count against this: it's not one of the request's own filters.
	ConsumedAll bool
	// OrderOK is true when every ORDER BY term was mapped to JQL ORDER BY (or
	// there were none).
	OrderOK bool
	// Defaulted is true when defaultBoundClause was applied because no filter
	// translated into a restriction clause.
	Defaulted bool
}

// jqlOrderFields maps a jira.issues column to its JQL ORDER BY field name.
var jqlOrderFields = map[string]string{
	"created":  "created",
	"updated":  "updated",
	"key":      "key",
	"priority": "priority",
	"due_date": "duedate",
	"status":   "status",
}

// buildJQL translates a ScanRequest into JQL for the enhanced search endpoint.
// Only the filters/columns documented in the package README are translated;
// anything else is left for SQLite's local re-filter (the superset rule).
func buildJQL(req source.ScanRequest) jqlPlan {
	var clauses []string
	consumedAll := true
	for _, f := range req.Filters {
		clause, ok := translateFilter(f)
		if ok {
			clauses = append(clauses, clause)
		} else {
			consumedAll = false
		}
	}

	defaulted := false
	if len(clauses) == 0 {
		clauses = []string{defaultBoundClause}
		defaulted = true
	}

	jql := strings.Join(clauses, " AND ")
	orderClause, orderOK := jqlOrderBy(req.OrderBy)
	if orderOK && orderClause != "" {
		jql += " ORDER BY " + orderClause
	}

	return jqlPlan{JQL: jql, ConsumedAll: consumedAll, OrderOK: orderOK, Defaulted: defaulted}
}

// translateFilter translates one WHERE conjunct into a JQL clause. ok is false
// when the column or operator isn't one of the supported push-down cases.
func translateFilter(f source.Filter) (string, bool) {
	switch f.Column {
	case "key":
		return translateKeyish(f, "key")
	case "project_key":
		return translateKeyish(f, "project")
	case "issue_type":
		return translateEq(f, "issuetype")
	case "status":
		return translateEq(f, "status")
	case "priority":
		return translateEq(f, "priority")
	case "resolution":
		return translateEq(f, "resolution")
	case "assignee_account_id":
		return translateEq(f, "assignee")
	case "reporter_account_id":
		return translateEq(f, "reporter")
	case "created":
		return translateRange(f, "created")
	case "updated":
		return translateRange(f, "updated")
	default:
		return "", false
	}
}

// translateEq translates a plain OpEq filter to `field = "value"`.
func translateEq(f source.Filter, field string) (string, bool) {
	if f.Op != sqlparse.OpEq {
		return "", false
	}
	s, ok := f.Value.(string)
	if !ok {
		return "", false
	}
	return field + " = " + quoteJQL(s), true
}

// translateKeyish translates an OpEq or OpIn filter on a key-like column (issue
// key, project key) to `field = "value"` or `field in ("a", "b", ...)`.
func translateKeyish(f source.Filter, field string) (string, bool) {
	switch f.Op {
	case sqlparse.OpEq:
		s, ok := f.Value.(string)
		if !ok {
			return "", false
		}
		return field + " = " + quoteJQL(s), true
	case sqlparse.OpIn:
		if len(f.Values) == 0 {
			return "", false
		}
		vals := make([]string, 0, len(f.Values))
		for _, v := range f.Values {
			s, ok := v.(string)
			if !ok {
				return "", false
			}
			vals = append(vals, quoteJQL(s))
		}
		return field + " in (" + strings.Join(vals, ", ") + ")", true
	default:
		return "", false
	}
}

// translateRange translates a range filter (Gt/Gte/Lt/Lte/Between) on a
// created/updated column to a JQL bound. JQL datetime granularity is minutes,
// so lower bounds are truncated down and upper bounds rounded up to the
// minute — the pushed clause is always a superset of the exact predicate,
// which SQLite re-applies locally.
func translateRange(f source.Filter, field string) (string, bool) {
	switch f.Op {
	case sqlparse.OpGt, sqlparse.OpGte:
		t, ok := parseJQLTime(f.Value)
		if !ok {
			return "", false
		}
		return field + " >= " + quoteJQLTime(floorMinute(t)), true
	case sqlparse.OpLt, sqlparse.OpLte:
		t, ok := parseJQLTime(f.Value)
		if !ok {
			return "", false
		}
		return field + " <= " + quoteJQLTime(ceilMinute(t)), true
	case sqlparse.OpBetween:
		if len(f.Values) != 2 {
			return "", false
		}
		lo, okLo := parseJQLTime(f.Values[0])
		hi, okHi := parseJQLTime(f.Values[1])
		if !okLo || !okHi {
			return "", false
		}
		return field + " >= " + quoteJQLTime(floorMinute(lo)) +
			" AND " + field + " <= " + quoteJQLTime(ceilMinute(hi)), true
	default:
		return "", false
	}
}

// parseJQLTime parses a created/updated literal. Columns are stored as
// RFC3339-ish TEXT verbatim from the API, but a WHERE clause may also use the
// friendlier "2006-01-02 15:04" or "2006-01-02" forms.
func parseJQLTime(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	if t, err := time.Parse("2006-01-02 15:04", s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// floorMinute truncates t down to the start of its minute.
func floorMinute(t time.Time) time.Time { return t.Truncate(time.Minute) }

// ceilMinute rounds t up to the start of the next minute, unless it already
// falls exactly on one.
func ceilMinute(t time.Time) time.Time {
	floored := t.Truncate(time.Minute)
	if floored.Equal(t) {
		return floored
	}
	return floored.Add(time.Minute)
}

func quoteJQLTime(t time.Time) string { return `"` + t.UTC().Format(jqlTimeLayout) + `"` }

// jqlOrderBy maps every ORDER BY term to a JQL ORDER BY field; ok is false
// (pushing no ordering) unless every term maps — a partially-mapped multi-key
// ORDER BY can't be honored upstream.
func jqlOrderBy(terms []source.OrderTerm) (string, bool) {
	if len(terms) == 0 {
		return "", true
	}
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		field, ok := jqlOrderFields[t.Column]
		if !ok {
			return "", false
		}
		dir := "ASC"
		if t.Desc {
			dir = "DESC"
		}
		parts = append(parts, field+" "+dir)
	}
	return strings.Join(parts, ", "), true
}

// quoteJQL wraps s in double quotes, escaping backslashes and embedded double
// quotes. JQL string comparisons are case-insensitive, so a quoted literal
// pushed upstream returns a superset of a case-sensitive SQLite match — safe.
func quoteJQL(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r == '\\' || r == '"' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// --- Atlassian Document Format -> plain text ---

// adfBlockTypes are ADF node types that end a line when rendered as plain text.
var adfBlockTypes = map[string]bool{
	"paragraph": true, "heading": true, "listItem": true, "codeBlock": true,
	"blockquote": true, "tableRow": true, "tableCell": true, "tableHeader": true,
	"panel": true, "rule": true,
}

// adfText renders an Atlassian Document Format value (as decoded JSON: a
// map[string]any/[]any tree) to plain text: text nodes are concatenated,
// hardBreak becomes a newline, mention nodes render their attrs.text, and
// block-level nodes (paragraph, heading, listItem, codeBlock, ...) are
// separated by a single newline. v may be nil (no description/body).
func adfText(v any) string {
	var b strings.Builder
	adfWalk(&b, v)
	return strings.TrimSpace(b.String())
}

func adfWalk(b *strings.Builder, v any) {
	switch n := v.(type) {
	case []any:
		for _, item := range n {
			adfWalk(b, item)
		}
	case map[string]any:
		typ, _ := n["type"].(string)
		switch typ {
		case "text":
			if s, ok := n["text"].(string); ok {
				b.WriteString(s)
			}
			return
		case "hardBreak":
			b.WriteString("\n")
			return
		case "mention":
			if attrs, ok := n["attrs"].(map[string]any); ok {
				if s, ok := attrs["text"].(string); ok {
					b.WriteString(s)
				}
			}
			return
		}
		if content, ok := n["content"].([]any); ok {
			adfWalk(b, content)
		}
		if adfBlockTypes[typ] && !endsWithNewline(b) {
			b.WriteString("\n")
		}
	}
}

func endsWithNewline(b *strings.Builder) bool {
	s := b.String()
	return s != "" && s[len(s)-1] == '\n'
}

// adfFieldText decodes a raw ADF JSON field (a description or comment body)
// into plain text. It returns nil (SQL NULL) when the field is absent or
// explicitly null, so callers can distinguish "no description" from "an empty
// one".
func adfFieldText(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil || v == nil {
		return nil
	}
	return adfText(v)
}
