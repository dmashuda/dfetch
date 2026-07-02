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

// Column names mirror the GitHub API JSON fields (created_at, updated_at, …) so
// queries read the same way as the API docs. ORDER BY on a timestamp column maps
// to the API's short sort name (created/updated/…) internally via orderParam.
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

var commitsCols = []source.Column{
	col("owner", "TEXT"), col("repo", "TEXT"), col("path", "TEXT"), col("sha", "TEXT"),
	col("message", "TEXT"), col("author_login", "TEXT"), col("author_name", "TEXT"),
	col("author_email", "TEXT"), col("author_date", "TEXT"),
	col("committer_login", "TEXT"), col("committer_name", "TEXT"),
	col("committer_date", "TEXT"), col("html_url", "TEXT"),
}

var releasesCols = []source.Column{
	col("owner", "TEXT"), col("repo", "TEXT"), col("tag_name", "TEXT"),
	col("name", "TEXT"), col("draft", "INTEGER"), col("prerelease", "INTEGER"),
	col("created_at", "TEXT"), col("published_at", "TEXT"), col("author_login", "TEXT"),
	col("html_url", "TEXT"), col("body", "TEXT"),
}

var workflowRunsCols = []source.Column{
	col("owner", "TEXT"), col("repo", "TEXT"), col("id", "INTEGER"),
	col("name", "TEXT"), col("head_branch", "TEXT"), col("head_sha", "TEXT"),
	col("run_number", "INTEGER"), col("event", "TEXT"), col("status", "TEXT"),
	col("conclusion", "TEXT"), col("workflow_id", "INTEGER"), col("actor_login", "TEXT"),
	col("created_at", "TEXT"), col("updated_at", "TEXT"), col("run_started_at", "TEXT"),
	col("html_url", "TEXT"),
}

var artifactsCols = []source.Column{
	col("owner", "TEXT"), col("repo", "TEXT"), col("id", "INTEGER"),
	col("name", "TEXT"), col("size_in_bytes", "INTEGER"), col("expired", "INTEGER"),
	col("created_at", "TEXT"), col("updated_at", "TEXT"), col("expires_at", "TEXT"),
	col("digest", "TEXT"), col("workflow_run_id", "INTEGER"), col("head_branch", "TEXT"),
	col("head_sha", "TEXT"), col("archive_download_url", "TEXT"),
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
	Name        string `json:"name"`
	FullName    string `json:"full_name"`
	Description string `json:"description"`
	Language    string `json:"language"`
	Stars       int64  `json:"stargazers_count"`
	Forks       int64  `json:"forks_count"`
	OpenIssues  int64  `json:"open_issues_count"`
	Private     bool   `json:"private"`
	HTMLURL     string `json:"html_url"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	PushedAt    string `json:"pushed_at"`
}

// commit endpoint nests the git-level author/committer (name/email/date) under
// "commit", separate from the top-level GitHub user accounts.
type ghCommitIdent struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type ghCommitDetail struct {
	Author    *ghCommitIdent `json:"author"`
	Committer *ghCommitIdent `json:"committer"`
	Message   string         `json:"message"`
}

type ghCommit struct {
	SHA       string         `json:"sha"`
	HTMLURL   string         `json:"html_url"`
	Commit    ghCommitDetail `json:"commit"`
	Author    *ghUser        `json:"author"`
	Committer *ghUser        `json:"committer"`
}

type ghRelease struct {
	TagName     string  `json:"tag_name"`
	Name        string  `json:"name"`
	Draft       bool    `json:"draft"`
	Prerelease  bool    `json:"prerelease"`
	CreatedAt   string  `json:"created_at"`
	PublishedAt *string `json:"published_at"`
	Author      *ghUser `json:"author"`
	HTMLURL     string  `json:"html_url"`
	Body        string  `json:"body"`
}

type ghRun struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	HeadBranch   string  `json:"head_branch"`
	HeadSHA      string  `json:"head_sha"`
	RunNumber    int64   `json:"run_number"`
	Event        string  `json:"event"`
	Status       string  `json:"status"`
	Conclusion   *string `json:"conclusion"`
	WorkflowID   int64   `json:"workflow_id"`
	Actor        *ghUser `json:"actor"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	RunStartedAt string  `json:"run_started_at"`
	HTMLURL      string  `json:"html_url"`
}

// the actions/runs endpoint wraps the list in an object (unlike issues/pulls).
type ghRunsResponse struct {
	WorkflowRuns []ghRun `json:"workflow_runs"`
}

type ghArtifact struct {
	ID                 int64   `json:"id"`
	Name               string  `json:"name"`
	SizeInBytes        int64   `json:"size_in_bytes"`
	Expired            bool    `json:"expired"`
	CreatedAt          *string `json:"created_at"`
	UpdatedAt          *string `json:"updated_at"`
	ExpiresAt          *string `json:"expires_at"`
	Digest             *string `json:"digest"`
	ArchiveDownloadURL string  `json:"archive_download_url"`
	WorkflowRun        *struct {
		ID         int64  `json:"id"`
		HeadBranch string `json:"head_branch"`
		HeadSHA    string `json:"head_sha"`
	} `json:"workflow_run"`
}

// the actions/artifacts endpoints wrap the list in an object, like runs.
type ghArtifactsResponse struct {
	Artifacts []ghArtifact `json:"artifacts"`
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

func (c *Connector) scanIssues(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	owner, err := requireStringEq(req, "owner")
	if err != nil {
		return err
	}
	repo, err := requireStringEq(req, "repo")
	if err != nil {
		return err
	}

	q := url.Values{}
	// The API defaults to state=open; without an explicit state the connector
	// must return every issue (a superset), not just the open ones.
	q.Set("state", "all")
	if state, ok := stringEq(req, "state"); ok {
		q.Set("state", state)
	}
	sort, dir, sortOK := orderParam(req.OrderBy, map[string]string{
		"created_at": "created", "updated_at": "updated", "comments": "comments",
	})
	if sortOK {
		q.Set("sort", sort)
		q.Set("direction", dir)
	}
	// The issues endpoint interleaves pull requests with the issues and offers
	// no server-side way to exclude them (only the pull_request marker on each
	// item), so PRs are dropped client-side below. That breaks pageLimit's
	// assumptions: a page may contribute fewer than per_page rows, so per_page
	// stays at a full page regardless of the pushed LIMIT (a limit-sized page
	// could be all PRs); and a pushed LIMIT may never be satisfied, so instead
	// of paging unconditionally until stopAt, the scan gives up (with a warning)
	// after maxPages consecutive pages that yielded no issues, rather than
	// walking a PR-heavy repo's entire history.
	_, stopAt, pushLimit := pageLimit(req, limitSafe(req, sortOK, "owner", "repo", "state"))
	q.Set("per_page", "100")

	start := c.baseURL + "/repos/" + escapePath(owner) + "/" + escapePath(repo) + "/issues?" + q.Encode()

	sent, idle := 0, 0
	next := start
	for pages := 0; next != "" && (pushLimit || pages < maxPages); pages++ {
		var items []ghIssue
		next, err = c.getJSON(ctx, next, &items)
		if err != nil {
			return err
		}
		page := make([][]any, 0, len(items))
		for _, it := range items {
			if it.PullRequest != nil {
				continue // the issues endpoint also returns PRs; exclude them
			}
			page = append(page, []any{
				owner, repo, it.Number, it.Title, it.State, login(it.User),
				it.Comments, labelNames(it.Labels),
				it.CreatedAt, it.UpdatedAt, nullable(it.ClosedAt), it.Body, it.HTMLURL,
			})
			sent++
			if pushLimit && sent >= stopAt {
				break
			}
		}
		if len(page) > 0 {
			if err := emit(&source.Rows{Columns: colNames(issuesCols), Rows: page}); err != nil {
				return err
			}
		}
		if err := pageCapped(pushLimit, pages, next, "issues", emit); err != nil {
			return err
		}
		if pushLimit && sent >= stopAt {
			return nil
		}
		if len(page) > 0 {
			idle = 0
			continue
		}
		idle++
		if pushLimit && idle >= maxPages && next != "" {
			return emit(source.Warn("github.issues: stopped after %d consecutive pages of pull requests only (the issues endpoint interleaves PRs, which don't count toward a LIMIT); results may be incomplete — narrow the filters", maxPages))
		}
	}
	return nil
}

// --- pulls ---

func (c *Connector) scanPulls(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	owner, err := requireStringEq(req, "owner")
	if err != nil {
		return err
	}
	repo, err := requireStringEq(req, "repo")
	if err != nil {
		return err
	}

	q := url.Values{}
	// The API defaults to state=open; without an explicit state the connector
	// must return every pull request (a superset), not just the open ones.
	q.Set("state", "all")
	if state, ok := stringEq(req, "state"); ok {
		q.Set("state", state)
	}
	sort, dir, sortOK := orderParam(req.OrderBy, map[string]string{
		"created_at": "created", "updated_at": "updated",
	})
	if sortOK {
		q.Set("sort", sort)
		q.Set("direction", dir)
	}
	perPage, stopAt, pushLimit := pageLimit(req, limitSafe(req, sortOK, "owner", "repo", "state"))
	q.Set("per_page", strconv.Itoa(perPage))

	start := c.baseURL + "/repos/" + escapePath(owner) + "/" + escapePath(repo) + "/pulls?" + q.Encode()

	sent := 0
	for next, pages := start, 0; next != "" && (pushLimit || pages < maxPages); pages++ {
		var items []ghPull
		next, err = c.getJSON(ctx, next, &items)
		if err != nil {
			return err
		}
		page := make([][]any, 0, len(items))
		for _, it := range items {
			page = append(page, []any{
				owner, repo, it.Number, it.Title, it.State, login(it.User),
				it.Draft, it.CreatedAt, it.UpdatedAt, nullable(it.MergedAt), it.Body, it.HTMLURL,
			})
			sent++
			if pushLimit && sent >= stopAt {
				break
			}
		}
		if len(page) > 0 {
			if err := emit(&source.Rows{Columns: colNames(pullsCols), Rows: page}); err != nil {
				return err
			}
		}
		if err := pageCapped(pushLimit, pages, next, "pulls", emit); err != nil {
			return err
		}
		if pushLimit && sent >= stopAt {
			return nil
		}
	}
	return nil
}

// --- repos ---

func (c *Connector) scanRepos(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	owner, err := requireStringEq(req, "owner")
	if err != nil {
		return err
	}

	// A name/repo filter selects a single repository; otherwise list the owner's.
	name, hasName := stringEq(req, "name")
	if !hasName {
		name, hasName = stringEq(req, "repo")
	}
	if hasName {
		var r ghRepo
		if _, err := c.getJSON(ctx, c.baseURL+"/repos/"+escapePath(owner)+"/"+escapePath(name), &r); err != nil {
			return err
		}
		return emit(&source.Rows{Columns: colNames(reposCols), Rows: [][]any{repoRow(owner, r)}})
	}

	q := url.Values{}
	sort, dir, sortOK := orderParam(req.OrderBy, map[string]string{
		"created_at": "created", "updated_at": "updated", "pushed_at": "pushed",
		"full_name": "full_name", "name": "full_name",
	})
	if sortOK {
		q.Set("sort", sort)
		q.Set("direction", dir)
	}
	perPage, stopAt, pushLimit := pageLimit(req, limitSafe(req, sortOK, "owner", "name", "repo"))
	q.Set("per_page", strconv.Itoa(perPage))

	start := c.baseURL + "/users/" + escapePath(owner) + "/repos?" + q.Encode()

	sent := 0
	for next, pages := start, 0; next != "" && (pushLimit || pages < maxPages); pages++ {
		var items []ghRepo
		next, err = c.getJSON(ctx, next, &items)
		if err != nil {
			return err
		}
		page := make([][]any, 0, len(items))
		for _, it := range items {
			page = append(page, repoRow(owner, it))
			sent++
			if pushLimit && sent >= stopAt {
				break
			}
		}
		if len(page) > 0 {
			if err := emit(&source.Rows{Columns: colNames(reposCols), Rows: page}); err != nil {
				return err
			}
		}
		if err := pageCapped(pushLimit, pages, next, "repos", emit); err != nil {
			return err
		}
		if pushLimit && sent >= stopAt {
			return nil
		}
	}
	return nil
}

func repoRow(owner string, r ghRepo) []any {
	// Store the owner exactly as the caller filtered on it (not the API's
	// canonical casing from r.owner.login), so the engine's verbatim
	// WHERE owner = '<as written>' still matches this row under SQLite's
	// case-sensitive default collation.
	return []any{
		owner, r.Name, r.FullName, r.Description, r.Language, r.Stars, r.Forks,
		r.OpenIssues, r.Private, r.HTMLURL, r.CreatedAt, r.UpdatedAt, r.PushedAt,
	}
}

// --- commits ---

func (c *Connector) scanCommits(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	owner, err := requireStringEq(req, "owner")
	if err != nil {
		return err
	}
	repo, err := requireStringEq(req, "repo")
	if err != nil {
		return err
	}

	q := url.Values{}
	// sha selects a branch/ref to list from (and its ancestors), so it's a
	// starting point rather than an exact match — pushed for efficiency but
	// excluded from limitSafe below so it can't enable a LIMIT push-down.
	if sha, ok := stringEq(req, "sha"); ok {
		q.Set("sha", sha)
	}
	// path is a filter-only API param: a commit touches many files, so the
	// response carries no single path. Echo the filtered value into the synthetic
	// "path" column (like owner/repo) so the engine's verbatim WHERE path = '…'
	// still matches; it's NULL when unfiltered.
	var pathVal any
	if path, ok := stringEq(req, "path"); ok {
		q.Set("path", path)
		pathVal = path
	}
	if author, ok := stringEq(req, "author_login"); ok {
		q.Set("author", author)
	}
	// The commits endpoint has no sort param (results are newest-first), so an
	// ORDER BY can't be honored upstream — pass sortMapped=false.
	perPage, stopAt, pushLimit := pageLimit(req, limitSafe(req, false, "owner", "repo", "path", "author_login"))
	q.Set("per_page", strconv.Itoa(perPage))

	start := c.baseURL + "/repos/" + escapePath(owner) + "/" + escapePath(repo) + "/commits?" + q.Encode()

	sent := 0
	for next, pages := start, 0; next != "" && (pushLimit || pages < maxPages); pages++ {
		var items []ghCommit
		next, err = c.getJSON(ctx, next, &items)
		if err != nil {
			return err
		}
		page := make([][]any, 0, len(items))
		for _, it := range items {
			page = append(page, commitRow(owner, repo, pathVal, it))
			sent++
			if pushLimit && sent >= stopAt {
				break
			}
		}
		if len(page) > 0 {
			if err := emit(&source.Rows{Columns: colNames(commitsCols), Rows: page}); err != nil {
				return err
			}
		}
		if err := pageCapped(pushLimit, pages, next, "commits", emit); err != nil {
			return err
		}
		if pushLimit && sent >= stopAt {
			return nil
		}
	}
	return nil
}

func commitRow(owner, repo string, path any, c ghCommit) []any {
	var authorName, authorEmail, authorDate, committerName, committerDate any
	if a := c.Commit.Author; a != nil {
		authorName, authorEmail, authorDate = a.Name, a.Email, a.Date
	}
	if cm := c.Commit.Committer; cm != nil {
		committerName, committerDate = cm.Name, cm.Date
	}
	return []any{
		owner, repo, path, c.SHA, c.Commit.Message, login(c.Author), authorName, authorEmail,
		authorDate, login(c.Committer), committerName, committerDate, c.HTMLURL,
	}
}

// --- releases ---

func (c *Connector) scanReleases(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	owner, err := requireStringEq(req, "owner")
	if err != nil {
		return err
	}
	repo, err := requireStringEq(req, "repo")
	if err != nil {
		return err
	}

	// The releases endpoint takes no filter/sort params (results are newest-first).
	q := url.Values{}
	perPage, stopAt, pushLimit := pageLimit(req, limitSafe(req, false, "owner", "repo"))
	q.Set("per_page", strconv.Itoa(perPage))

	start := c.baseURL + "/repos/" + escapePath(owner) + "/" + escapePath(repo) + "/releases?" + q.Encode()

	sent := 0
	for next, pages := start, 0; next != "" && (pushLimit || pages < maxPages); pages++ {
		var items []ghRelease
		next, err = c.getJSON(ctx, next, &items)
		if err != nil {
			return err
		}
		page := make([][]any, 0, len(items))
		for _, it := range items {
			page = append(page, []any{
				owner, repo, it.TagName, it.Name, it.Draft, it.Prerelease,
				it.CreatedAt, nullable(it.PublishedAt), login(it.Author), it.HTMLURL, it.Body,
			})
			sent++
			if pushLimit && sent >= stopAt {
				break
			}
		}
		if len(page) > 0 {
			if err := emit(&source.Rows{Columns: colNames(releasesCols), Rows: page}); err != nil {
				return err
			}
		}
		if err := pageCapped(pushLimit, pages, next, "releases", emit); err != nil {
			return err
		}
		if pushLimit && sent >= stopAt {
			return nil
		}
	}
	return nil
}

// --- workflow runs ---

func (c *Connector) scanWorkflowRuns(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	owner, err := requireStringEq(req, "owner")
	if err != nil {
		return err
	}
	repo, err := requireStringEq(req, "repo")
	if err != nil {
		return err
	}

	q := url.Values{}
	if branch, ok := stringEq(req, "head_branch"); ok {
		q.Set("branch", branch)
	}
	if event, ok := stringEq(req, "event"); ok {
		q.Set("event", event)
	}
	if status, ok := stringEq(req, "status"); ok {
		q.Set("status", status)
	}
	if actor, ok := stringEq(req, "actor_login"); ok {
		q.Set("actor", actor)
	}
	if headSHA, ok := stringEq(req, "head_sha"); ok {
		q.Set("head_sha", headSHA)
	}
	// No sort param (results are newest-first), so an ORDER BY can't be honored.
	perPage, stopAt, pushLimit := pageLimit(req, limitSafe(req, false,
		"owner", "repo", "head_branch", "event", "status", "actor_login", "head_sha"))
	q.Set("per_page", strconv.Itoa(perPage))

	start := c.baseURL + "/repos/" + escapePath(owner) + "/" + escapePath(repo) + "/actions/runs?" + q.Encode()

	sent := 0
	for next, pages := start, 0; next != "" && (pushLimit || pages < maxPages); pages++ {
		var resp ghRunsResponse
		next, err = c.getJSON(ctx, next, &resp)
		if err != nil {
			return err
		}
		page := make([][]any, 0, len(resp.WorkflowRuns))
		for _, it := range resp.WorkflowRuns {
			page = append(page, []any{
				owner, repo, it.ID, it.Name, it.HeadBranch, it.HeadSHA, it.RunNumber,
				it.Event, it.Status, nullable(it.Conclusion), it.WorkflowID, login(it.Actor),
				it.CreatedAt, it.UpdatedAt, it.RunStartedAt, it.HTMLURL,
			})
			sent++
			if pushLimit && sent >= stopAt {
				break
			}
		}
		if len(page) > 0 {
			if err := emit(&source.Rows{Columns: colNames(workflowRunsCols), Rows: page}); err != nil {
				return err
			}
		}
		if err := pageCapped(pushLimit, pages, next, "workflow_runs", emit); err != nil {
			return err
		}
		if pushLimit && sent >= stopAt {
			return nil
		}
	}
	return nil
}

// --- artifacts ---

func (c *Connector) scanArtifacts(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	owner, err := requireStringEq(req, "owner")
	if err != nil {
		return err
	}
	repo, err := requireStringEq(req, "repo")
	if err != nil {
		return err
	}

	// A workflow_run_id filter selects one run's artifacts via the run-scoped
	// endpoint; otherwise list the whole repo's. Both honor an exact name filter.
	path := "/actions/artifacts"
	if runID, ok := intEq(req, "workflow_run_id"); ok {
		path = "/actions/runs/" + strconv.FormatInt(runID, 10) + "/artifacts"
	}

	q := url.Values{}
	if name, ok := stringEq(req, "name"); ok {
		q.Set("name", name)
	}
	// No sort param mapped (results are newest-first), so an ORDER BY can't be honored.
	perPage, stopAt, pushLimit := pageLimit(req, limitSafe(req, false,
		"owner", "repo", "name", "workflow_run_id"))
	q.Set("per_page", strconv.Itoa(perPage))

	start := c.baseURL + "/repos/" + escapePath(owner) + "/" + escapePath(repo) + path + "?" + q.Encode()

	sent := 0
	for next, pages := start, 0; next != "" && (pushLimit || pages < maxPages); pages++ {
		var resp ghArtifactsResponse
		next, err = c.getJSON(ctx, next, &resp)
		if err != nil {
			return err
		}
		page := make([][]any, 0, len(resp.Artifacts))
		for _, it := range resp.Artifacts {
			page = append(page, artifactRow(owner, repo, it))
			sent++
			if pushLimit && sent >= stopAt {
				break
			}
		}
		if len(page) > 0 {
			if err := emit(&source.Rows{Columns: colNames(artifactsCols), Rows: page}); err != nil {
				return err
			}
		}
		if err := pageCapped(pushLimit, pages, next, "artifacts", emit); err != nil {
			return err
		}
		if pushLimit && sent >= stopAt {
			return nil
		}
	}
	return nil
}

func artifactRow(owner, repo string, a ghArtifact) []any {
	var runID, headBranch, headSHA any
	if r := a.WorkflowRun; r != nil {
		runID, headBranch, headSHA = r.ID, r.HeadBranch, r.HeadSHA
	}
	return []any{
		owner, repo, a.ID, a.Name, a.SizeInBytes, a.Expired,
		nullable(a.CreatedAt), nullable(a.UpdatedAt), nullable(a.ExpiresAt), nullable(a.Digest),
		runID, headBranch, headSHA, a.ArchiveDownloadURL,
	}
}

// pageCapped emits a truncation warning when an uncapped scan stops on its final
// allowed page (maxPages) while the API still has more pages, so the user knows
// the result is a subset. It is a no-op when a LIMIT was pushed or the data was
// exhausted before the cap.
func pageCapped(pushLimit bool, pages int, next, table string, emit func(*source.Rows) error) error {
	if pushLimit || pages != maxPages-1 || next == "" {
		return nil
	}
	return emit(source.Warn("github.%s: stopped at the %d-page cap; results may be incomplete — add a LIMIT or narrower filters", table, maxPages))
}

func colNames(cols []source.Column) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}
