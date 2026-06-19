package github

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"github.com/dmashuda/dfetch/internal/source"
)

func col(name, typ string) source.Column { return source.Column{Name: name, Type: typ} }

var issuesCols = []source.Column{
	col("owner", "TEXT"), col("repo", "TEXT"), col("number", "INTEGER"),
	col("title", "TEXT"), col("state", "TEXT"), col("user_login", "TEXT"),
	col("comments", "INTEGER"), col("labels", "TEXT"),
	col("created_at", "TEXT"), col("updated_at", "TEXT"), col("closed_at", "TEXT"),
	col("body", "TEXT"), col("html_url", "TEXT"),
}

var pullsCols = []source.Column{
	col("owner", "TEXT"), col("repo", "TEXT"), col("number", "INTEGER"),
	col("title", "TEXT"), col("state", "TEXT"), col("user_login", "TEXT"),
	col("draft", "INTEGER"), col("created_at", "TEXT"), col("updated_at", "TEXT"),
	col("merged_at", "TEXT"), col("body", "TEXT"), col("html_url", "TEXT"),
}

var reposCols = []source.Column{
	col("owner", "TEXT"), col("name", "TEXT"), col("full_name", "TEXT"),
	col("description", "TEXT"), col("language", "TEXT"), col("stars", "INTEGER"),
	col("forks", "INTEGER"), col("open_issues", "INTEGER"), col("private", "INTEGER"),
	col("html_url", "TEXT"), col("created_at", "TEXT"), col("updated_at", "TEXT"),
	col("pushed_at", "TEXT"),
}

// --- JSON shapes ---

type ghUser struct {
	Login string `json:"login"`
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghIssue struct {
	Number      int64            `json:"number"`
	Title       string           `json:"title"`
	State       string           `json:"state"`
	User        *ghUser          `json:"user"`
	Comments    int64            `json:"comments"`
	Labels      []ghLabel        `json:"labels"`
	CreatedAt   string           `json:"created_at"`
	UpdatedAt   string           `json:"updated_at"`
	ClosedAt    *string          `json:"closed_at"`
	Body        string           `json:"body"`
	HTMLURL     string           `json:"html_url"`
	PullRequest *json.RawMessage `json:"pull_request"`
}

type ghPull struct {
	Number    int64   `json:"number"`
	Title     string  `json:"title"`
	State     string  `json:"state"`
	User      *ghUser `json:"user"`
	Draft     bool    `json:"draft"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	MergedAt  *string `json:"merged_at"`
	Body      string  `json:"body"`
	HTMLURL   string  `json:"html_url"`
}

type ghRepo struct {
	Name        string  `json:"name"`
	FullName    string  `json:"full_name"`
	Description string  `json:"description"`
	Language    string  `json:"language"`
	Stars       int64   `json:"stargazers_count"`
	Forks       int64   `json:"forks_count"`
	OpenIssues  int64   `json:"open_issues_count"`
	Private     bool    `json:"private"`
	HTMLURL     string  `json:"html_url"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	PushedAt    string  `json:"pushed_at"`
	Owner       *ghUser `json:"owner"`
}

// --- helpers ---

func login(u *ghUser) any {
	if u == nil {
		return nil
	}
	return u.Login
}

func labelNames(labels []ghLabel) string {
	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Name
	}
	return strings.Join(names, ",")
}

func nullable(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// --- issues ---

func (c *Connector) scanIssues(ctx context.Context, req source.ScanRequest) (*source.Rows, error) {
	owner, err := requireStringEq(req, "owner")
	if err != nil {
		return nil, err
	}
	repo, err := requireStringEq(req, "repo")
	if err != nil {
		return nil, err
	}

	q := url.Values{}
	if state, ok := stringEq(req, "state"); ok {
		q.Set("state", state)
	}
	sort, dir, sortOK := orderParam(req.OrderBy, map[string]string{
		"created": "created", "created_at": "created",
		"updated": "updated", "updated_at": "updated",
		"comments": "comments",
	})
	if sortOK {
		q.Set("sort", sort)
		q.Set("direction", dir)
	}
	perPage, pushLimit := pageLimit(req, sortOK)
	q.Set("per_page", strconv.Itoa(perPage))

	start := c.baseURL + "/repos/" + escapePath(owner) + "/" + escapePath(repo) + "/issues?" + q.Encode()

	var rows [][]any
	for next, pages := start, 0; next != "" && pages < maxPages; pages++ {
		var items []ghIssue
		next, err = c.getJSON(ctx, next, &items)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.PullRequest != nil {
				continue // the issues endpoint also returns PRs; exclude them
			}
			rows = append(rows, []any{
				owner, repo, it.Number, it.Title, it.State, login(it.User),
				it.Comments, labelNames(it.Labels),
				it.CreatedAt, it.UpdatedAt, nullable(it.ClosedAt), it.Body, it.HTMLURL,
			})
			if pushLimit && len(rows) >= *req.Limit {
				return &source.Rows{Columns: colNames(issuesCols), Rows: rows}, nil
			}
		}
	}
	return &source.Rows{Columns: colNames(issuesCols), Rows: rows}, nil
}

// --- pulls ---

func (c *Connector) scanPulls(ctx context.Context, req source.ScanRequest) (*source.Rows, error) {
	owner, err := requireStringEq(req, "owner")
	if err != nil {
		return nil, err
	}
	repo, err := requireStringEq(req, "repo")
	if err != nil {
		return nil, err
	}

	q := url.Values{}
	if state, ok := stringEq(req, "state"); ok {
		q.Set("state", state)
	}
	sort, dir, sortOK := orderParam(req.OrderBy, map[string]string{
		"created": "created", "created_at": "created",
		"updated": "updated", "updated_at": "updated",
	})
	if sortOK {
		q.Set("sort", sort)
		q.Set("direction", dir)
	}
	perPage, pushLimit := pageLimit(req, sortOK)
	q.Set("per_page", strconv.Itoa(perPage))

	start := c.baseURL + "/repos/" + escapePath(owner) + "/" + escapePath(repo) + "/pulls?" + q.Encode()

	var rows [][]any
	for next, pages := start, 0; next != "" && pages < maxPages; pages++ {
		var items []ghPull
		next, err = c.getJSON(ctx, next, &items)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			rows = append(rows, []any{
				owner, repo, it.Number, it.Title, it.State, login(it.User),
				it.Draft, it.CreatedAt, it.UpdatedAt, nullable(it.MergedAt), it.Body, it.HTMLURL,
			})
			if pushLimit && len(rows) >= *req.Limit {
				return &source.Rows{Columns: colNames(pullsCols), Rows: rows}, nil
			}
		}
	}
	return &source.Rows{Columns: colNames(pullsCols), Rows: rows}, nil
}

// --- repos ---

func (c *Connector) scanRepos(ctx context.Context, req source.ScanRequest) (*source.Rows, error) {
	owner, err := requireStringEq(req, "owner")
	if err != nil {
		return nil, err
	}

	// A name/repo filter selects a single repository; otherwise list the owner's.
	name, hasName := stringEq(req, "name")
	if !hasName {
		name, hasName = stringEq(req, "repo")
	}
	if hasName {
		var r ghRepo
		if _, err := c.getJSON(ctx, c.baseURL+"/repos/"+escapePath(owner)+"/"+escapePath(name), &r); err != nil {
			return nil, err
		}
		return &source.Rows{Columns: colNames(reposCols), Rows: [][]any{repoRow(owner, r)}}, nil
	}

	q := url.Values{}
	sort, dir, sortOK := orderParam(req.OrderBy, map[string]string{
		"created": "created", "created_at": "created",
		"updated": "updated", "updated_at": "updated",
		"pushed": "pushed", "pushed_at": "pushed",
		"full_name": "full_name", "name": "full_name",
	})
	if sortOK {
		q.Set("sort", sort)
		q.Set("direction", dir)
	}
	perPage, pushLimit := pageLimit(req, sortOK)
	q.Set("per_page", strconv.Itoa(perPage))

	start := c.baseURL + "/users/" + escapePath(owner) + "/repos?" + q.Encode()

	var rows [][]any
	for next, pages := start, 0; next != "" && pages < maxPages; pages++ {
		var items []ghRepo
		next, err = c.getJSON(ctx, next, &items)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			rows = append(rows, repoRow(owner, it))
			if pushLimit && len(rows) >= *req.Limit {
				return &source.Rows{Columns: colNames(reposCols), Rows: rows}, nil
			}
		}
	}
	return &source.Rows{Columns: colNames(reposCols), Rows: rows}, nil
}

func repoRow(owner string, r ghRepo) []any {
	o := owner
	if r.Owner != nil && r.Owner.Login != "" {
		o = r.Owner.Login
	}
	return []any{
		o, r.Name, r.FullName, r.Description, r.Language, r.Stars, r.Forks,
		r.OpenIssues, r.Private, r.HTMLURL, r.CreatedAt, r.UpdatedAt, r.PushedAt,
	}
}

func colNames(cols []source.Column) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}
