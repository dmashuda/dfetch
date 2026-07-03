package newrelic

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/sqlparse"
)

// Curated tables: fixed schemas served from NerdGraph object queries (as
// opposed to the dynamic NRDB event types). Column names mirror the NerdGraph
// fields, lowercased/underscored. Timestamps are INTEGER epoch ms; list/map
// fields are JSON text (query with json_extract). NerdGraph ID scalars arrive
// as JSON strings and stay TEXT.

func col(name, typ string) source.Column { return source.Column{Name: name, Type: typ} }

var accountsCols = []source.Column{
	col("id", "INTEGER"), col("name", "TEXT"),
}

var entitiesCols = []source.Column{
	col("guid", "TEXT"), col("name", "TEXT"), col("domain", "TEXT"),
	col("type", "TEXT"), col("entity_type", "TEXT"), col("account_id", "INTEGER"),
	col("reporting", "INTEGER"), col("alert_severity", "TEXT"),
	col("permalink", "TEXT"), col("tags", "TEXT"),
}

var policiesCols = []source.Column{
	col("id", "TEXT"), col("name", "TEXT"), col("incident_preference", "TEXT"),
	col("account_id", "INTEGER"),
}

var conditionsCols = []source.Column{
	col("id", "TEXT"), col("name", "TEXT"), col("policy_id", "TEXT"),
	col("enabled", "INTEGER"), col("type", "TEXT"), col("nrql_query", "TEXT"),
	col("account_id", "INTEGER"),
}

var issuesCols = []source.Column{
	col("issue_id", "TEXT"), col("title", "TEXT"), col("priority", "TEXT"),
	col("state", "TEXT"), col("created_at", "INTEGER"), col("closed_at", "INTEGER"),
	col("entity_names", "TEXT"), col("entity_types", "TEXT"), col("account_id", "INTEGER"),
}

var incidentsCols = []source.Column{
	col("incident_id", "TEXT"), col("title", "TEXT"), col("description", "TEXT"),
	col("priority", "TEXT"), col("state", "TEXT"), col("created_at", "INTEGER"),
	col("closed_at", "INTEGER"), col("entity_guids", "TEXT"), col("account_id", "INTEGER"),
}

// curatedTables maps curated table names to their fixed schemas. DescribeTable
// checks it before any NRDB keyset lookup, so these names always win over a
// same-named event type (implausible anyway: event types are conventionally
// PascalCase, these are snake_case).
var curatedTables = map[string][]source.Column{
	"accounts":         accountsCols,
	"entities":         entitiesCols,
	"alert_policies":   policiesCols,
	"alert_conditions": conditionsCols,
	"issues":           issuesCols,
	"incidents":        incidentsCols,
}

// stringEq returns the string value of an equality filter on column, if any.
func stringEq(req source.ScanRequest, column string) (string, bool) {
	f, ok := req.Filter(column)
	if !ok || f.Op != sqlparse.OpEq {
		return "", false
	}
	s, ok := f.Value.(string)
	return s, ok
}

// --- accounts ---

const accountsGQL = `{ actor { accounts { id name } } }`

func (c *Connector) scanAccounts(ctx context.Context, emit func(*source.Rows) error) error {
	var out struct {
		Actor struct {
			Accounts []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"accounts"`
		} `json:"actor"`
	}
	if err := c.gqlPost(ctx, accountsGQL, nil, &out); err != nil {
		return err
	}
	rows := make([][]any, 0, len(out.Actor.Accounts))
	for _, a := range out.Actor.Accounts {
		rows = append(rows, []any{a.ID, a.Name})
	}
	if len(rows) == 0 {
		return nil
	}
	return emit(&source.Rows{Columns: colNames(accountsCols), Rows: rows})
}

// --- entities ---

const entitiesGQL = `query($q: String!, $cursor: String) {
  actor { entitySearch(query: $q) { results(cursor: $cursor) {
    entities { guid name entityType domain type accountId reporting alertSeverity permalink tags { key values } }
    nextCursor
  } } }
}`

// buildEntitySearchQuery renders the entitySearch query string. It always
// scopes to the configured account (which also guarantees a non-empty query),
// then ANDs equality filters the search understands. Every pushed term only
// narrows the fetch — entity-search matching semantics (case sensitivity) are
// not guaranteed to equal SQLite's, so none of them count as consumed and a
// LIMIT never rides them.
func buildEntitySearchQuery(req source.ScanRequest, account int64) string {
	parts := []string{fmt.Sprintf("accountId = %d", account)}
	for filterCol, searchKey := range map[string]string{
		"name": "name", "domain": "domain", "type": "type", "guid": "id",
	} {
		if v, ok := stringEq(req, filterCol); ok {
			parts = append(parts, fmt.Sprintf("%s = '%s'", searchKey, escapeNRQLString(v)))
		}
	}
	// Deterministic order for tests: accountId first, then sorted terms.
	sort.Strings(parts[1:])
	return strings.Join(parts, " AND ")
}

func (c *Connector) scanEntities(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	q := buildEntitySearchQuery(req, c.account)
	stopAt, pushLimit := pageLimit(req, limitSafe(req)) // no consumed columns: filters only narrow

	fetch := func(ctx context.Context, cursor string) ([][]any, string, error) {
		var out struct {
			Actor struct {
				EntitySearch struct {
					Results *struct {
						Entities []struct {
							GUID          string  `json:"guid"`
							Name          string  `json:"name"`
							EntityType    string  `json:"entityType"`
							Domain        string  `json:"domain"`
							Type          string  `json:"type"`
							AccountID     int64   `json:"accountId"`
							Reporting     bool    `json:"reporting"`
							AlertSeverity *string `json:"alertSeverity"`
							Permalink     string  `json:"permalink"`
							Tags          []struct {
								Key    string   `json:"key"`
								Values []string `json:"values"`
							} `json:"tags"`
						} `json:"entities"`
						NextCursor *string `json:"nextCursor"`
					} `json:"results"`
				} `json:"entitySearch"`
			} `json:"actor"`
		}
		vars := map[string]any{"q": q}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		if err := c.gqlPost(ctx, entitiesGQL, vars, &out); err != nil {
			return nil, "", err
		}
		res := out.Actor.EntitySearch.Results
		if res == nil {
			return nil, "", nil
		}
		rows := make([][]any, 0, len(res.Entities))
		for _, e := range res.Entities {
			tags := map[string][]string{}
			for _, t := range e.Tags {
				tags[t.Key] = t.Values
			}
			tagJSON, _ := json.Marshal(tags)
			rows = append(rows, []any{
				e.GUID, e.Name, e.Domain, e.Type, e.EntityType, e.AccountID,
				e.Reporting, ptrOrNil(e.AlertSeverity), e.Permalink, string(tagJSON),
			})
		}
		return rows, strPtr(res.NextCursor), nil
	}
	return cursorPages(ctx, "entities", entitiesCols, stopAt, pushLimit, fetch, emit)
}

// --- alert policies ---

const policiesGQL = `query($account: Int!, $cursor: String) {
  actor { account(id: $account) { alerts { policiesSearch(cursor: $cursor) {
    policies { id name incidentPreference } nextCursor
  } } } }
}`

func (c *Connector) scanPolicies(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	stopAt, pushLimit := pageLimit(req, limitSafe(req))

	fetch := func(ctx context.Context, cursor string) ([][]any, string, error) {
		var out struct {
			Actor struct {
				Account struct {
					Alerts struct {
						PoliciesSearch *struct {
							Policies []struct {
								ID                 string `json:"id"`
								Name               string `json:"name"`
								IncidentPreference string `json:"incidentPreference"`
							} `json:"policies"`
							NextCursor *string `json:"nextCursor"`
						} `json:"policiesSearch"`
					} `json:"alerts"`
				} `json:"account"`
			} `json:"actor"`
		}
		vars := map[string]any{"account": c.account}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		if err := c.gqlPost(ctx, policiesGQL, vars, &out); err != nil {
			return nil, "", err
		}
		res := out.Actor.Account.Alerts.PoliciesSearch
		if res == nil {
			return nil, "", nil
		}
		rows := make([][]any, 0, len(res.Policies))
		for _, p := range res.Policies {
			rows = append(rows, []any{p.ID, p.Name, p.IncidentPreference, c.account})
		}
		return rows, strPtr(res.NextCursor), nil
	}
	return cursorPages(ctx, "alert_policies", policiesCols, stopAt, pushLimit, fetch, emit)
}

// --- alert conditions ---

const conditionsGQL = `query($account: Int!, $cursor: String, $criteria: AlertsNrqlConditionsSearchCriteriaInput) {
  actor { account(id: $account) { alerts { nrqlConditionsSearch(cursor: $cursor, searchCriteria: $criteria) {
    nrqlConditions { id name policyId enabled type nrql { query } } nextCursor
  } } } }
}`

func (c *Connector) scanConditions(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	// policy_id equality is an exact ID match on the search criteria, so it is
	// consumed and a LIMIT may ride it — but only when it actually became
	// criteria: stringEq rejects a non-string value (policy_id = 123), and an
	// unsent filter must not count as consumed or the early stop would truncate
	// the unfiltered list before SQLite re-filters it.
	var criteria map[string]any
	consumed := []string{}
	if id, ok := stringEq(req, "policy_id"); ok {
		criteria = map[string]any{"policyId": id}
		consumed = append(consumed, "policy_id")
	}
	stopAt, pushLimit := pageLimit(req, limitSafe(req, consumed...))

	fetch := func(ctx context.Context, cursor string) ([][]any, string, error) {
		var out struct {
			Actor struct {
				Account struct {
					Alerts struct {
						NrqlConditionsSearch *struct {
							NrqlConditions []struct {
								ID       string `json:"id"`
								Name     string `json:"name"`
								PolicyID string `json:"policyId"`
								Enabled  bool   `json:"enabled"`
								Type     string `json:"type"`
								Nrql     *struct {
									Query string `json:"query"`
								} `json:"nrql"`
							} `json:"nrqlConditions"`
							NextCursor *string `json:"nextCursor"`
						} `json:"nrqlConditionsSearch"`
					} `json:"alerts"`
				} `json:"account"`
			} `json:"actor"`
		}
		vars := map[string]any{"account": c.account}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		if criteria != nil {
			vars["criteria"] = criteria
		}
		if err := c.gqlPost(ctx, conditionsGQL, vars, &out); err != nil {
			// NerdGraph reports a searchCriteria policyId that doesn't exist as
			// an error ("Policy with ID <id> not found"), but under SQL semantics
			// `WHERE policy_id = '<id>'` on a missing policy is an empty result,
			// not a failure. Match that exact error narrowly (verified live);
			// anything else stays fatal.
			if id, ok := criteria["policyId"].(string); ok &&
				strings.Contains(err.Error(), fmt.Sprintf("Policy with ID %s not found", id)) {
				return nil, "", nil
			}
			return nil, "", err
		}
		res := out.Actor.Account.Alerts.NrqlConditionsSearch
		if res == nil {
			return nil, "", nil
		}
		rows := make([][]any, 0, len(res.NrqlConditions))
		for _, n := range res.NrqlConditions {
			var nrql any
			if n.Nrql != nil {
				nrql = n.Nrql.Query
			}
			rows = append(rows, []any{n.ID, n.Name, n.PolicyID, n.Enabled, n.Type, nrql, c.account})
		}
		return rows, strPtr(res.NextCursor), nil
	}
	return cursorPages(ctx, "alert_conditions", conditionsCols, stopAt, pushLimit, fetch, emit)
}

// --- issues / incidents ---

const issuesGQL = `query($account: Int!, $cursor: String) {
  actor { account(id: $account) { aiIssues { issues(cursor: $cursor) {
    issues { issueId title priority state createdAt closedAt entityNames entityTypes } nextCursor
  } } } }
}`

func (c *Connector) scanIssues(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	stopAt, pushLimit := pageLimit(req, limitSafe(req))

	fetch := func(ctx context.Context, cursor string) ([][]any, string, error) {
		var out struct {
			Actor struct {
				Account struct {
					AiIssues struct {
						Issues *struct {
							Issues []struct {
								IssueID     string      `json:"issueId"`
								Title       flexString  `json:"title"`
								Priority    flexString  `json:"priority"`
								State       flexString  `json:"state"`
								CreatedAt   *float64    `json:"createdAt"`
								ClosedAt    *float64    `json:"closedAt"`
								EntityNames flexStrings `json:"entityNames"`
								EntityTypes flexStrings `json:"entityTypes"`
							} `json:"issues"`
							NextCursor *string `json:"nextCursor"`
						} `json:"issues"`
					} `json:"aiIssues"`
				} `json:"account"`
			} `json:"actor"`
		}
		vars := map[string]any{"account": c.account}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		if err := c.gqlPost(ctx, issuesGQL, vars, &out); err != nil {
			return nil, "", err
		}
		res := out.Actor.Account.AiIssues.Issues
		if res == nil {
			return nil, "", nil
		}
		rows := make([][]any, 0, len(res.Issues))
		for _, i := range res.Issues {
			rows = append(rows, []any{
				i.IssueID, strOrNil(string(i.Title)), strOrNil(string(i.Priority)), strOrNil(string(i.State)),
				msInt(i.CreatedAt), msInt(i.ClosedAt), i.EntityNames.jsonOrNil(), i.EntityTypes.jsonOrNil(), c.account,
			})
		}
		return rows, strPtr(res.NextCursor), nil
	}
	return cursorPages(ctx, "issues", issuesCols, stopAt, pushLimit, fetch, emit)
}

const incidentsGQL = `query($account: Int!, $cursor: String) {
  actor { account(id: $account) { aiIssues { incidents(cursor: $cursor) {
    incidents { incidentId title description priority state createdAt closedAt entityGuids } nextCursor
  } } } }
}`

func (c *Connector) scanIncidents(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	stopAt, pushLimit := pageLimit(req, limitSafe(req))

	fetch := func(ctx context.Context, cursor string) ([][]any, string, error) {
		var out struct {
			Actor struct {
				Account struct {
					AiIssues struct {
						Incidents *struct {
							Incidents []struct {
								IncidentID  string      `json:"incidentId"`
								Title       flexString  `json:"title"`
								Description flexString  `json:"description"`
								Priority    flexString  `json:"priority"`
								State       flexString  `json:"state"`
								CreatedAt   *float64    `json:"createdAt"`
								ClosedAt    *float64    `json:"closedAt"`
								EntityGuids flexStrings `json:"entityGuids"`
							} `json:"incidents"`
							NextCursor *string `json:"nextCursor"`
						} `json:"incidents"`
					} `json:"aiIssues"`
				} `json:"account"`
			} `json:"actor"`
		}
		vars := map[string]any{"account": c.account}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		if err := c.gqlPost(ctx, incidentsGQL, vars, &out); err != nil {
			return nil, "", err
		}
		res := out.Actor.Account.AiIssues.Incidents
		if res == nil {
			return nil, "", nil
		}
		rows := make([][]any, 0, len(res.Incidents))
		for _, i := range res.Incidents {
			rows = append(rows, []any{
				i.IncidentID, strOrNil(string(i.Title)), strOrNil(string(i.Description)),
				strOrNil(string(i.Priority)), strOrNil(string(i.State)),
				msInt(i.CreatedAt), msInt(i.ClosedAt), i.EntityGuids.jsonOrNil(), c.account,
			})
		}
		return rows, strPtr(res.NextCursor), nil
	}
	return cursorPages(ctx, "incidents", incidentsCols, stopAt, pushLimit, fetch, emit)
}

// --- decode helpers ---

// flexString decodes a JSON string or an array of strings (first element):
// several aiIssues fields are documented as [String] and the exact shape is
// deployment-dependent.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err == nil {
		if len(arr) > 0 {
			*f = flexString(arr[0])
		}
		return nil
	}
	return fmt.Errorf("expected string or [string], got %s", truncate(b, 40))
}

// flexStrings decodes a JSON string or array of strings and holds it as a JSON
// array literal, stored in a TEXT column for json_each/json_extract.
type flexStrings string

func (f *flexStrings) UnmarshalJSON(b []byte) error {
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("expected [string] or string, got %s", truncate(b, 40))
		}
		arr = []string{s}
	}
	j, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	*f = flexStrings(j)
	return nil
}

// jsonOrNil returns the held JSON array text, or nil when nothing decoded.
func (f flexStrings) jsonOrNil() any {
	if f == "" {
		return nil
	}
	return string(f)
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func ptrOrNil(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

func strPtr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// msInt converts a float epoch-ms (NerdGraph number) to int64, nil when absent.
func msInt(v *float64) any {
	if v == nil {
		return nil
	}
	return int64(*v)
}
