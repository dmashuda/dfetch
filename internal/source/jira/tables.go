package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/sqlparse"
)

func col(name, typ string) source.Column { return source.Column{Name: name, Type: typ} }

// issueSearchFields is sent explicitly on every /search/jql request: the
// endpoint defaults to id-only when fields is omitted.
var issueSearchFields = []string{
	"summary", "description", "status", "issuetype", "priority", "resolution",
	"assignee", "reporter", "labels", "created", "updated", "resolutiondate",
	"duedate", "project", "parent",
}

var issuesCols = []source.Column{
	col("id", "INTEGER"), col("key", "TEXT"), col("project_key", "TEXT"),
	col("issue_type", "TEXT"), col("status", "TEXT"), col("status_category", "TEXT"),
	col("priority", "TEXT"), col("resolution", "TEXT"), col("summary", "TEXT"),
	col("description", "TEXT"),
	col("assignee_account_id", "TEXT"), col("assignee_display_name", "TEXT"),
	col("reporter_account_id", "TEXT"), col("reporter_display_name", "TEXT"),
	col("labels", "TEXT"),
	col("created", "TEXT"), col("updated", "TEXT"), col("resolved", "TEXT"), col("due_date", "TEXT"),
	col("parent_key", "TEXT"), col("url", "TEXT"),
}

var projectsCols = []source.Column{
	col("id", "INTEGER"), col("key", "TEXT"), col("name", "TEXT"),
	col("project_type_key", "TEXT"), col("style", "TEXT"), col("is_private", "INTEGER"),
	col("url", "TEXT"),
}

var commentsCols = []source.Column{
	col("issue_key", "TEXT"), col("id", "INTEGER"),
	col("author_account_id", "TEXT"), col("author_display_name", "TEXT"),
	col("body", "TEXT"), col("created", "TEXT"), col("updated", "TEXT"),
	col("is_public", "INTEGER"),
}

// --- JSON shapes ---

type jiraUser struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
}

type jiraNamed struct {
	Name string `json:"name"`
}

type jiraStatus struct {
	Name           string    `json:"name"`
	StatusCategory jiraNamed `json:"statusCategory"`
}

// jiraKeyRef is any sub-object referenced by issue key (project, parent).
type jiraKeyRef struct {
	Key string `json:"key"`
}

type jiraIssueFields struct {
	Summary        string          `json:"summary"`
	Description    json.RawMessage `json:"description"`
	Status         jiraStatus      `json:"status"`
	IssueType      jiraNamed       `json:"issuetype"`
	Priority       *jiraNamed      `json:"priority"`
	Resolution     *jiraNamed      `json:"resolution"`
	Assignee       *jiraUser       `json:"assignee"`
	Reporter       *jiraUser       `json:"reporter"`
	Labels         []string        `json:"labels"`
	Created        string          `json:"created"`
	Updated        string          `json:"updated"`
	ResolutionDate *string         `json:"resolutiondate"`
	DueDate        *string         `json:"duedate"`
	Project        jiraKeyRef      `json:"project"`
	Parent         *jiraKeyRef     `json:"parent"`
}

type jiraIssue struct {
	ID     string          `json:"id"`
	Key    string          `json:"key"`
	Fields jiraIssueFields `json:"fields"`
}

// jiraSearchResponse is the /rest/api/3/search/jql response shape. NextPageToken
// is absent/null on the last page; there is no total.
type jiraSearchResponse struct {
	Issues        []jiraIssue `json:"issues"`
	NextPageToken *string     `json:"nextPageToken"`
}

type jiraProject struct {
	ID             string `json:"id"`
	Key            string `json:"key"`
	Name           string `json:"name"`
	ProjectTypeKey string `json:"projectTypeKey"`
	Style          string `json:"style"`
	IsPrivate      bool   `json:"isPrivate"`
	Self           string `json:"self"`
}

type jiraProjectSearchResponse struct {
	Values []jiraProject `json:"values"`
	IsLast bool          `json:"isLast"`
}

type jiraComment struct {
	ID        string          `json:"id"`
	Author    *jiraUser       `json:"author"`
	Body      json.RawMessage `json:"body"`
	Created   string          `json:"created"`
	Updated   string          `json:"updated"`
	JsdPublic *bool           `json:"jsdPublic"`
}

type jiraCommentsResponse struct {
	Comments []jiraComment `json:"comments"`
	Total    int           `json:"total"`
}

// --- helpers ---

func nullableStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func userField(u *jiraUser) (id, name any) {
	if u == nil {
		return nil, nil
	}
	return u.AccountID, u.DisplayName
}

func namedField(n *jiraNamed) any {
	if n == nil {
		return nil
	}
	return n.Name
}

func parseID(kind, ref, id string) (int64, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("jira: %s %s: invalid id %q: %w", kind, ref, id, err)
	}
	return n, nil
}

// --- issues ---

func (c *Connector) scanIssues(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	plan := buildJQL(req)
	// The boundedness default blocks LIMIT push too: it narrows the result to
	// 30 days of history the query never asked for, so a pushed LIMIT would
	// lock in the top-N of that window as if it were the top-N overall.
	limitOK := plan.ConsumedAll && plan.OrderOK && !plan.Defaulted
	stopAt, pushLimit := pageLimit(req, limitOK)
	maxResults := pageSize(stopAt, pushLimit)

	if plan.Defaulted {
		if err := emit(source.Warn("jira.issues: no filter translated to JQL, so the search was bounded to %s — filter on project_key, key, created, updated, or assignee/reporter account id for more history", defaultBoundClause)); err != nil {
			return err
		}
	}

	sent, token := 0, ""
	for pages := 0; pushLimit || pages < maxPages; pages++ {
		body := map[string]any{
			"jql":        plan.JQL,
			"maxResults": maxResults,
			"fields":     issueSearchFields,
		}
		if token != "" {
			body["nextPageToken"] = token
		}

		var resp jiraSearchResponse
		if err := c.postJSON(ctx, c.baseURL+"/rest/api/3/search/jql", body, &resp); err != nil {
			return err
		}

		rows := make([][]any, 0, len(resp.Issues))
		for _, it := range resp.Issues {
			row, err := issueRow(c.baseURL, it)
			if err != nil {
				return err
			}
			rows = append(rows, row)
			sent++
			if pushLimit && sent >= stopAt {
				break
			}
		}
		if len(rows) > 0 {
			if err := emit(&source.Rows{Columns: colNames(issuesCols), Rows: rows}); err != nil {
				return err
			}
		}

		// An empty page means no progress — treat it as terminal even if a
		// token came back, so a pathological response can't loop forever when
		// pushLimit disables the page cap.
		hasNext := resp.NextPageToken != nil && *resp.NextPageToken != "" && len(resp.Issues) > 0
		if err := pageCapped(pushLimit, pages, hasNext, "issues", emit); err != nil {
			return err
		}
		if pushLimit && sent >= stopAt {
			return nil
		}
		if !hasNext {
			return nil
		}
		token = *resp.NextPageToken
	}
	return nil
}

func issueRow(baseURL string, it jiraIssue) ([]any, error) {
	id, err := parseID("issue", it.Key, it.ID)
	if err != nil {
		return nil, err
	}
	f := it.Fields

	assigneeID, assigneeName := userField(f.Assignee)
	reporterID, reporterName := userField(f.Reporter)
	var parentKey any
	if f.Parent != nil {
		parentKey = f.Parent.Key
	}
	labels, err := json.Marshal(f.Labels)
	if err != nil {
		return nil, fmt.Errorf("jira: issue %s: encoding labels: %w", it.Key, err)
	}

	return []any{
		id, it.Key, f.Project.Key, f.IssueType.Name, f.Status.Name, f.Status.StatusCategory.Name,
		namedField(f.Priority), namedField(f.Resolution), f.Summary, adfFieldText(f.Description),
		assigneeID, assigneeName, reporterID, reporterName,
		string(labels),
		f.Created, f.Updated, nullableStr(f.ResolutionDate), nullableStr(f.DueDate),
		parentKey, baseURL + "/browse/" + it.Key,
	}, nil
}

// --- projects ---

func (c *Connector) scanProjects(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	keys := stringEqOrIn(req, "key")

	sortBy, sortOK := orderParam(req.OrderBy, map[string]string{"key": "key", "name": "name"})
	stopAt, pushLimit := pageLimit(req, projectsLimitSafe(req, sortOK))
	maxResults := pageSize(stopAt, pushLimit)

	q := url.Values{}
	for _, k := range keys {
		q.Add("keys", k)
	}
	if sortOK {
		q.Set("orderBy", sortBy)
	}
	q.Set("maxResults", strconv.Itoa(maxResults))

	sent, startAt := 0, 0
	for pages := 0; pushLimit || pages < maxPages; pages++ {
		q.Set("startAt", strconv.Itoa(startAt))

		var resp jiraProjectSearchResponse
		if err := c.getJSON(ctx, c.baseURL+"/rest/api/3/project/search?"+q.Encode(), &resp); err != nil {
			return err
		}

		rows := make([][]any, 0, len(resp.Values))
		for _, p := range resp.Values {
			row, err := projectRow(p)
			if err != nil {
				return err
			}
			rows = append(rows, row)
			sent++
			if pushLimit && sent >= stopAt {
				break
			}
		}
		if len(rows) > 0 {
			if err := emit(&source.Rows{Columns: colNames(projectsCols), Rows: rows}); err != nil {
				return err
			}
		}

		hasNext := !resp.IsLast && len(resp.Values) > 0
		if err := pageCapped(pushLimit, pages, hasNext, "projects", emit); err != nil {
			return err
		}
		if pushLimit && sent >= stopAt {
			return nil
		}
		if !hasNext {
			return nil
		}
		startAt += len(resp.Values)
	}
	return nil
}

// projectsLimitSafe reports whether the request's at-most-one filter is an
// equality/IN on "key" (the only column the endpoint filters on) and the
// ordering (if any) was mapped — the conditions under which a LIMIT may ride
// the paginated fetch. Multiple filters are never limit-safe: only the first
// is pushed (stringEqOrIn), so a second one could re-filter away rows the
// truncated fetch already spent the LIMIT on. sortOK implies exactly one
// ORDER BY term (orderParam enforces it).
func projectsLimitSafe(req source.ScanRequest, sortOK bool) bool {
	if len(req.OrderBy) > 0 && !sortOK {
		return false
	}
	if len(req.Filters) > 1 {
		return false
	}
	for _, f := range req.Filters {
		if f.Column != "key" || (f.Op != sqlparse.OpEq && f.Op != sqlparse.OpIn) {
			return false
		}
	}
	return true
}

func projectRow(p jiraProject) ([]any, error) {
	id, err := parseID("project", p.Key, p.ID)
	if err != nil {
		return nil, err
	}
	return []any{id, p.Key, p.Name, p.ProjectTypeKey, p.Style, p.IsPrivate, p.Self}, nil
}

// --- comments ---

func (c *Connector) scanComments(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	keys, err := issueKeys(req)
	if err != nil {
		return err
	}

	sortBy, sortOK := orderParam(req.OrderBy, map[string]string{"created": "created"})
	stopAt, pushLimit := pageLimit(req, commentsLimitSafe(req, keys, sortOK))
	maxResults := pageSize(stopAt, pushLimit)

	q := url.Values{}
	if sortOK {
		q.Set("orderBy", sortBy)
	}
	q.Set("maxResults", strconv.Itoa(maxResults))

	sent := 0
	for _, key := range keys {
		startAt := 0
		for pages := 0; pushLimit || pages < maxPages; pages++ {
			q.Set("startAt", strconv.Itoa(startAt))

			var resp jiraCommentsResponse
			rawurl := c.baseURL + "/rest/api/3/issue/" + escapePath(key) + "/comment?" + q.Encode()
			if err := c.getJSON(ctx, rawurl, &resp); err != nil {
				return err
			}

			rows := make([][]any, 0, len(resp.Comments))
			for _, cm := range resp.Comments {
				row, err := commentRow(key, cm)
				if err != nil {
					return err
				}
				rows = append(rows, row)
				sent++
				if pushLimit && sent >= stopAt {
					break
				}
			}
			if len(rows) > 0 {
				if err := emit(&source.Rows{Columns: colNames(commentsCols), Rows: rows}); err != nil {
					return err
				}
			}

			// Guard on len > 0 like scanProjects: a zero-row page with total
			// still ahead (comments hidden by visibility restrictions, stale
			// total) must not loop forever once pushLimit disables the cap.
			hasNext := len(resp.Comments) > 0 && startAt+len(resp.Comments) < resp.Total
			if err := pageCapped(pushLimit, pages, hasNext, "comments", emit); err != nil {
				return err
			}
			if pushLimit && sent >= stopAt {
				return nil
			}
			if !hasNext {
				break
			}
			startAt += len(resp.Comments)
		}
	}
	return nil
}

// issueKeys returns the issue_key filter's value(s): a required equality or IN
// filter (an error is returned, without any HTTP request made, when it's
// absent).
func issueKeys(req source.ScanRequest) ([]string, error) {
	keys := stringEqOrIn(req, "issue_key")
	if len(keys) == 0 {
		return nil, errors.New("jira: comments requires an issue_key filter (e.g. WHERE issue_key = 'PROJ-1')")
	}
	return keys, nil
}

// commentsLimitSafe reports whether a LIMIT may ride the fetch: a single
// issue_key (multiple keys means multiple independent paginated fetches, which
// can't jointly honor one global ORDER BY + LIMIT), the ordering (if any) fully
// mapped (sortOK implies exactly one ORDER BY term — orderParam enforces it),
// and every filter besides issue_key absent.
func commentsLimitSafe(req source.ScanRequest, keys []string, sortOK bool) bool {
	if len(keys) != 1 {
		return false
	}
	if len(req.OrderBy) > 0 && !sortOK {
		return false
	}
	for _, f := range req.Filters {
		if f.Column != "issue_key" {
			return false
		}
	}
	return true
}

func commentRow(issueKey string, cm jiraComment) ([]any, error) {
	id, err := parseID("comment", issueKey, cm.ID)
	if err != nil {
		return nil, err
	}
	authorID, authorName := userField(cm.Author)
	var isPublic any
	if cm.JsdPublic != nil {
		isPublic = *cm.JsdPublic
	}
	return []any{
		issueKey, id, authorID, authorName, adfFieldText(cm.Body), cm.Created, cm.Updated, isPublic,
	}, nil
}
