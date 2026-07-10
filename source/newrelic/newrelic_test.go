package newrelic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmashuda/dfetch/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gqlCall is one captured NerdGraph request body.
type gqlCall struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// nrql returns the NRQL variable of a captured call ("" when absent).
func (c gqlCall) nrql() string {
	s, _ := c.Variables["nrql"].(string)
	return s
}

// capture wraps a handler to record every decoded request body and assert the
// auth header, returning the pointer to the captured slice.
func capture(t *testing.T, h func(w http.ResponseWriter, call gqlCall)) (*[]gqlCall, http.HandlerFunc) {
	t.Helper()
	calls := &[]gqlCall{}
	return calls, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "test-user-key", r.Header.Get("API-Key"))
		var call gqlCall
		require.NoError(t, json.NewDecoder(r.Body).Decode(&call))
		*calls = append(*calls, call)
		h(w, call)
	}
}

// nrqlResults writes a NerdGraph envelope holding NRQL results.
func nrqlResults(w http.ResponseWriter, resultsJSON string) {
	_, _ = fmt.Fprintf(w, `{"data":{"actor":{"account":{"nrql":{"results":%s}}}}}`, resultsJSON)
}

func newTestConnector(t *testing.T, params map[string]any, h http.HandlerFunc) *Connector {
	t.Helper()
	t.Setenv("NEW_RELIC_API_KEY", "test-user-key")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	if params == nil {
		params = map[string]any{}
	}
	params["base_url"] = srv.URL
	if _, ok := params["account_id"]; !ok {
		params["account_id"] = 1
	}
	c, err := New(params)
	require.NoError(t, err)
	return c.(*Connector)
}

func collectScan(c source.Connector, req source.ScanRequest) (*source.Rows, error) {
	rows := &source.Rows{}
	err := c.Scan(context.Background(), req, func(chunk *source.Rows) error {
		if rows.Columns == nil && len(chunk.Columns) > 0 {
			rows.Columns = chunk.Columns
		}
		rows.Rows = append(rows.Rows, chunk.Rows...)
		rows.Warnings = append(rows.Warnings, chunk.Warnings...)
		return nil
	})
	return rows, err
}

// keysetResponse is the canned Transaction keyset (array shape).
const keysetResponse = `[{"stringKeys":["appName"],"numericKeys":["duration","timestamp"],"booleanKeys":["error"]}]`

// nrdbHandler serves keyset and data queries for the Transaction event type.
func nrdbHandler(dataJSON string) func(w http.ResponseWriter, call gqlCall) {
	return func(w http.ResponseWriter, call gqlCall) {
		switch {
		case strings.Contains(call.nrql(), "keyset()"):
			nrqlResults(w, keysetResponse)
		case strings.Contains(call.nrql(), "SHOW EVENT TYPES"):
			nrqlResults(w, `[{"eventType":"Transaction"},{"eventType":"PageView"}]`)
		default:
			nrqlResults(w, dataJSON)
		}
	}
}

func TestNewValidation(t *testing.T) {
	t.Setenv("NEW_RELIC_API_KEY", "k")
	_, err := New(map[string]any{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "account_id")

	// Region selects the EU endpoint; base_url still wins for tests.
	c, err := New(map[string]any{"account_id": 1, "region": "eu"})
	require.NoError(t, err)
	assert.Equal(t, euBaseURL, c.(*Connector).baseURL)

	// max_rows is clamped to the NRQL hard cap.
	c, err = New(map[string]any{"account_id": 1, "max_rows": 999999})
	require.NoError(t, err)
	assert.Equal(t, nrqlHardCap, c.(*Connector).maxRows)

	// The HTTP timeout follows the configured NRQL timeout with headroom, so
	// a slow-but-allowed query isn't killed client-side first.
	c, err = New(map[string]any{"account_id": 1, "nrql_timeout": 120})
	require.NoError(t, err)
	assert.Equal(t, 120, c.(*Connector).nrqlTimeout)
	assert.Equal(t, 150*time.Second, c.(*Connector).client.Timeout)
}

// api_key_command supplies the key when the env vars are unset (the new
// command hatch); it is resolved lazily and sent as the API-Key header.
func TestAPIKeyFromCommand(t *testing.T) {
	t.Setenv("DFETCH_NEWRELIC_API_KEY", "")
	t.Setenv("NEW_RELIC_API_KEY", "")
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("API-Key")
		nrqlResults(w, `[{"eventType":"Transaction"}]`)
	}))
	t.Cleanup(srv.Close)

	c, err := New(map[string]any{
		"account_id":      1,
		"base_url":        srv.URL,
		"api_key_command": []any{"printf", "key-from-cmd"},
	})
	require.NoError(t, err)

	_, err = c.(*Connector).ListTables(context.Background(), source.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, "key-from-cmd", gotKey)
}

// A missing API key must not break construction (a committed config would
// otherwise fail every dfetch command for people without the env var); the
// helpful error surfaces only when a newrelic table is actually used.
func TestMissingKeyDeferredToUse(t *testing.T) {
	t.Setenv("DFETCH_NEWRELIC_API_KEY", "")
	t.Setenv("NEW_RELIC_API_KEY", "")
	c, err := New(map[string]any{"account_id": 1})
	require.NoError(t, err)

	// Curated schemas resolve without the API, so they still work keyless.
	_, found, err := c.(*Connector).DescribeTable(context.Background(), "entities")
	require.NoError(t, err)
	assert.True(t, found)

	// Anything that reaches NerdGraph errors with the key guidance.
	_, err = collectScan(c, source.ScanRequest{Table: "Span"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "User API key")

	_, err = c.(*Connector).ListTables(context.Background(), source.ListOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "User API key")
}

// Curated tables resolve with zero API calls and always win over event types.
func TestDescribeTableCuratedPrecedence(t *testing.T) {
	calls, h := capture(t, func(w http.ResponseWriter, call gqlCall) {
		t.Error("curated DescribeTable must not call the API")
	})
	c := newTestConnector(t, nil, h)

	ts, found, err := c.DescribeTable(context.Background(), "entities")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, entitiesCols, ts.Columns)
	assert.Empty(t, *calls)
}

func TestDescribeTableKeysetAndCache(t *testing.T) {
	calls, h := capture(t, nrdbHandler("[]"))
	c := newTestConnector(t, nil, h)

	ts, found, err := c.DescribeTable(context.Background(), "Transaction")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "timestamp", ts.Columns[0].Name)
	assert.Contains(t, (*calls)[0].nrql(), "SELECT keyset() FROM `Transaction` SINCE 1 day ago")

	// Second describe is served from the cache: no new request.
	_, _, err = c.DescribeTable(context.Background(), "Transaction")
	require.NoError(t, err)
	assert.Len(t, *calls, 1)
}

func TestDescribeTableUnknownAndUnsafeNames(t *testing.T) {
	calls, h := capture(t, func(w http.ResponseWriter, call gqlCall) {
		nrqlResults(w, `[]`) // keyset of an unknown type: no attributes
	})
	c := newTestConnector(t, nil, h)

	_, found, err := c.DescribeTable(context.Background(), "NopeType")
	require.NoError(t, err)
	assert.False(t, found)

	// A backticked name never reaches NRQL (injection guard).
	_, found, err = c.DescribeTable(context.Background(), "Bad`SELECT")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Len(t, *calls, 1) // only the NopeType keyset call
}

func TestListTablesMergesCurated(t *testing.T) {
	_, h := capture(t, nrdbHandler("[]"))
	c := newTestConnector(t, nil, h)

	names, err := c.ListTables(context.Background(), source.ListOptions{})
	require.NoError(t, err)
	assert.Contains(t, names, "Transaction")
	assert.Contains(t, names, "PageView")
	assert.Contains(t, names, "entities")
	assert.Contains(t, names, "accounts")

	// Filter + limit apply locally.
	names, err = c.ListTables(context.Background(), source.ListOptions{Filter: "trans", Limit: 5})
	require.NoError(t, err)
	assert.Equal(t, []string{"Transaction"}, names)
}

func TestScanNRDBMapsRows(t *testing.T) {
	calls, h := capture(t, nrdbHandler(`[
		{"timestamp": 1750000000500, "appName": "billing", "duration": 1.5, "error": false,
		 "nested": {"a": 1}},
		{"timestamp": 1750000001000, "appName": "api"}
	]`))
	c := newTestConnector(t, nil, h)

	rows, err := collectScan(c, source.ScanRequest{Table: "Transaction"})
	require.NoError(t, err)
	assert.Equal(t, []string{"timestamp", "appName", "duration", "error"}, rows.Columns)
	require.Len(t, rows.Rows, 2)

	assert.Equal(t, int64(1750000000500), rows.Rows[0][0]) // float ms -> int64
	assert.Equal(t, "billing", rows.Rows[0][1])
	assert.Equal(t, 1.5, rows.Rows[0][2])
	assert.Equal(t, false, rows.Rows[0][3])
	assert.Nil(t, rows.Rows[1][2]) // missing attribute -> NULL

	// keyset first, then the data query.
	require.Len(t, *calls, 2)
	assert.Contains(t, (*calls)[1].nrql(), "SELECT * FROM `Transaction`")
}

func TestScanNRDBPushdownLanded(t *testing.T) {
	calls, h := capture(t, nrdbHandler("[]"))
	c := newTestConnector(t, nil, h)

	limit := 50
	_, err := collectScan(c, source.ScanRequest{
		Table:   "Transaction",
		Columns: []string{"appName", "timestamp"},
		Filters: []source.Filter{
			{Column: "appName", Op: source.OpEq, Value: "billing"},
			{Column: "timestamp", Op: source.OpGte, Value: int64(1750000000000)},
		},
		OrderBy: []source.OrderTerm{{Column: "timestamp", Desc: true}},
		Limit:   &limit,
	})
	require.NoError(t, err)

	nrql := (*calls)[1].nrql()
	assert.Contains(t, nrql, "SELECT `appName`, `timestamp` FROM `Transaction`")
	assert.Contains(t, nrql, "WHERE `appName` = 'billing' AND `timestamp` >= 1750000000000")
	assert.Contains(t, nrql, "ORDER BY `timestamp` DESC")
	assert.Contains(t, nrql, "LIMIT 50")
	assert.Contains(t, nrql, "SINCE 1749999999999")
}

func TestScanNRDBLimitNotPushedForUnhonoredOrder(t *testing.T) {
	calls, h := capture(t, nrdbHandler("[]"))
	c := newTestConnector(t, nil, h)

	limit := 5
	_, err := collectScan(c, source.ScanRequest{
		Table:   "Transaction",
		OrderBy: []source.OrderTerm{{Column: "appName"}},
		Limit:   &limit,
	})
	require.NoError(t, err)
	nrql := (*calls)[1].nrql()
	assert.NotContains(t, nrql, "ORDER BY")
	assert.Contains(t, nrql, "LIMIT 5000")
}

func TestScanNRDBWarnings(t *testing.T) {
	// max_rows 2 and exactly 2 rows returned: the cap likely truncated.
	calls, h := capture(t, nrdbHandler(`[{"timestamp":1},{"timestamp":2}]`))
	c := newTestConnector(t, map[string]any{"max_rows": 2}, h)

	rows, err := collectScan(c, source.ScanRequest{Table: "Transaction"})
	require.NoError(t, err)
	require.Len(t, rows.Warnings, 2)
	assert.Contains(t, rows.Warnings[0], "capped at 2 rows")
	assert.Contains(t, rows.Warnings[1], "no timestamp lower bound")
	assert.Contains(t, (*calls)[1].nrql(), "LIMIT 2")

	// With a timestamp lower bound and results under the cap: no warnings.
	_, h2 := capture(t, nrdbHandler(`[{"timestamp":1750000000100}]`))
	c2 := newTestConnector(t, nil, h2)
	rows, err = collectScan(c2, source.ScanRequest{
		Table:   "Transaction",
		Filters: []source.Filter{{Column: "timestamp", Op: source.OpGte, Value: int64(1750000000000)}},
	})
	require.NoError(t, err)
	assert.Empty(t, rows.Warnings)
}

func TestScanNRDBUnknownTable(t *testing.T) {
	_, h := capture(t, func(w http.ResponseWriter, call gqlCall) { nrqlResults(w, `[]`) })
	c := newTestConnector(t, nil, h)

	_, err := collectScan(c, source.ScanRequest{Table: "NopeType"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no event type "NopeType"`)
}

func TestGraphQLErrorsAreFatal(t *testing.T) {
	_, h := capture(t, func(w http.ResponseWriter, call gqlCall) {
		// Partial data alongside errors must not be loaded silently.
		_, _ = fmt.Fprint(w, `{"data":{"actor":{"account":{"nrql":{"results":[{"timestamp":1}]}}}},"errors":[{"message":"NRQL Syntax Error at line 1"}]}`)
	})
	c := newTestConnector(t, nil, h)

	_, err := collectScan(c, source.ScanRequest{Table: "Transaction"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NRQL Syntax Error")
}

func TestHTTPErrorSurfaced(t *testing.T) {
	t.Setenv("NEW_RELIC_API_KEY", "test-user-key")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	c, err := New(map[string]any{"base_url": srv.URL, "account_id": 1})
	require.NoError(t, err)

	_, err = collectScan(c, source.ScanRequest{Table: "Transaction"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
	assert.Contains(t, err.Error(), "User API key")
}

// --- curated tables ---

func TestScanAccounts(t *testing.T) {
	_, h := capture(t, func(w http.ResponseWriter, call gqlCall) {
		_, _ = fmt.Fprint(w, `{"data":{"actor":{"accounts":[{"id":1,"name":"prod"},{"id":2,"name":"dev"}]}}}`)
	})
	c := newTestConnector(t, nil, h)

	rows, err := collectScan(c, source.ScanRequest{Table: "accounts"})
	require.NoError(t, err)
	assert.Equal(t, []string{"id", "name"}, rows.Columns)
	require.Len(t, rows.Rows, 2)
	assert.Equal(t, []any{int64(1), "prod"}, rows.Rows[0])
}

func TestScanEntitiesPaginatesAndPushesSearch(t *testing.T) {
	page := func(cursor, next string) string {
		nextJSON := "null"
		if next != "" {
			nextJSON = fmt.Sprintf("%q", next)
		}
		return fmt.Sprintf(`{"data":{"actor":{"entitySearch":{"results":{
			"entities":[{"guid":"G-%s","name":"svc","entityType":"APM_APPLICATION_ENTITY","domain":"APM","type":"APPLICATION",
			             "accountId":1,"reporting":true,"alertSeverity":"NOT_ALERTING","permalink":"https://x","tags":[{"key":"env","values":["prod"]}]}],
			"nextCursor":%s}}}}}`, cursor, nextJSON)
	}
	calls, h := capture(t, func(w http.ResponseWriter, call gqlCall) {
		if cur, ok := call.Variables["cursor"].(string); ok {
			_, _ = fmt.Fprint(w, page(cur, ""))
			return
		}
		_, _ = fmt.Fprint(w, page("first", "c1"))
	})
	c := newTestConnector(t, nil, h)

	rows, err := collectScan(c, source.ScanRequest{
		Table:   "entities",
		Filters: []source.Filter{{Column: "name", Op: source.OpEq, Value: "svc"}},
	})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 2) // both pages
	assert.Equal(t, "G-first", rows.Rows[0][0])
	assert.Equal(t, "G-c1", rows.Rows[1][0])
	assert.JSONEq(t, `{"env":["prod"]}`, rows.Rows[0][9].(string))

	// The search query carries the account scope and the pushed name filter.
	q, _ := (*calls)[0].Variables["q"].(string)
	assert.Equal(t, "accountId = 1 AND name = 'svc'", q)
	// Second request resumed from the cursor.
	assert.Equal(t, "c1", (*calls)[1].Variables["cursor"])
}

// entity-search pushes only narrow; a LIMIT with any filter must not stop
// pagination early (SQLite re-filters, so early truncation could drop rows).
func TestScanEntitiesFilterBlocksEarlyStop(t *testing.T) {
	pages := 0
	_, h := capture(t, func(w http.ResponseWriter, call gqlCall) {
		pages++
		next := "null"
		if pages < 3 {
			next = fmt.Sprintf("%q", fmt.Sprintf("c%d", pages))
		}
		_, _ = fmt.Fprintf(w, `{"data":{"actor":{"entitySearch":{"results":{
			"entities":[{"guid":"G%d","name":"svc","accountId":1}],"nextCursor":%s}}}}}`, pages, next)
	})
	c := newTestConnector(t, nil, h)

	limit := 1
	rows, err := collectScan(c, source.ScanRequest{
		Table:   "entities",
		Filters: []source.Filter{{Column: "name", Op: source.OpEq, Value: "svc"}},
		Limit:   &limit,
	})
	require.NoError(t, err)
	assert.Equal(t, 3, pages)   // did NOT stop at the limit
	assert.Len(t, rows.Rows, 3) // superset delivered; SQLite trims

	// Without filters the limit may stop pagination early.
	pages = 0
	_, h2 := capture(t, func(w http.ResponseWriter, call gqlCall) {
		pages++
		_, _ = fmt.Fprintf(w, `{"data":{"actor":{"entitySearch":{"results":{
			"entities":[{"guid":"G%d","name":"svc","accountId":1}],"nextCursor":"more"}}}}}`, pages)
	})
	c2 := newTestConnector(t, nil, h2)
	rows, err = collectScan(c2, source.ScanRequest{Table: "entities", Limit: &limit})
	require.NoError(t, err)
	assert.Equal(t, 1, pages)
	assert.Len(t, rows.Rows, 1)
}

func TestScanEntitiesPageCapWarns(t *testing.T) {
	_, h := capture(t, func(w http.ResponseWriter, call gqlCall) {
		_, _ = fmt.Fprint(w, `{"data":{"actor":{"entitySearch":{"results":{
			"entities":[{"guid":"G","name":"svc","accountId":1}],"nextCursor":"again"}}}}}`)
	})
	c := newTestConnector(t, nil, h)

	rows, err := collectScan(c, source.ScanRequest{Table: "entities"})
	require.NoError(t, err)
	assert.Len(t, rows.Rows, maxPages)
	require.NotEmpty(t, rows.Warnings)
	assert.Contains(t, rows.Warnings[0], "page cap")
}

func TestScanConditionsPushesPolicyID(t *testing.T) {
	calls, h := capture(t, func(w http.ResponseWriter, call gqlCall) {
		_, _ = fmt.Fprint(w, `{"data":{"actor":{"account":{"alerts":{"nrqlConditionsSearch":{
			"nrqlConditions":[{"id":"7","name":"cpu","policyId":"123","enabled":true,"type":"STATIC","nrql":{"query":"SELECT ..."}}],
			"nextCursor":null}}}}}}`)
	})
	c := newTestConnector(t, nil, h)

	limit := 1
	rows, err := collectScan(c, source.ScanRequest{
		Table:   "alert_conditions",
		Filters: []source.Filter{{Column: "policy_id", Op: source.OpEq, Value: "123"}},
		Limit:   &limit,
	})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	assert.Equal(t, []any{"7", "cpu", "123", true, "STATIC", "SELECT ...", int64(1)}, rows.Rows[0])

	criteria, _ := (*calls)[0].Variables["criteria"].(map[string]any)
	require.NotNil(t, criteria)
	assert.Equal(t, "123", criteria["policyId"])
}

// A non-string policy_id equality (policy_id = 123) can't become search
// criteria, so it is not consumed: the LIMIT must not stop pagination early
// over the unfiltered list (SQLite re-filters, so truncation would drop rows).
func TestScanConditionsNonStringPolicyIDBlocksEarlyStop(t *testing.T) {
	pages := 0
	calls, h := capture(t, func(w http.ResponseWriter, call gqlCall) {
		pages++
		next := "null"
		if pages < 3 {
			next = fmt.Sprintf("%q", fmt.Sprintf("c%d", pages))
		}
		_, _ = fmt.Fprintf(w, `{"data":{"actor":{"account":{"alerts":{"nrqlConditionsSearch":{
			"nrqlConditions":[{"id":"%d","name":"c","policyId":"999","enabled":true,"type":"STATIC"}],
			"nextCursor":%s}}}}}}`, pages, next)
	})
	c := newTestConnector(t, nil, h)

	limit := 1
	rows, err := collectScan(c, source.ScanRequest{
		Table:   "alert_conditions",
		Filters: []source.Filter{{Column: "policy_id", Op: source.OpEq, Value: int64(123)}},
		Limit:   &limit,
	})
	require.NoError(t, err)
	assert.Equal(t, 3, pages)   // did NOT stop at the limit
	assert.Len(t, rows.Rows, 3) // superset delivered; SQLite trims
	_, hasCriteria := (*calls)[0].Variables["criteria"]
	assert.False(t, hasCriteria) // the int filter never became criteria
}

// A searchCriteria policyId that doesn't exist comes back as a GraphQL error,
// but under SQL semantics `WHERE policy_id = ...` on a missing policy is an
// empty result, not a failure (error text verified against the live API).
func TestScanConditionsMissingPolicyIsEmpty(t *testing.T) {
	_, h := capture(t, func(w http.ResponseWriter, call gqlCall) {
		_, _ = fmt.Fprint(w, `{"errors":[{"message":"Policy with ID 42 not found"}]}`)
	})
	c := newTestConnector(t, nil, h)

	rows, err := collectScan(c, source.ScanRequest{
		Table:   "alert_conditions",
		Filters: []source.Filter{{Column: "policy_id", Op: source.OpEq, Value: "42"}},
	})
	require.NoError(t, err)
	assert.Empty(t, rows.Rows)

	// Any other GraphQL error stays fatal.
	_, h2 := capture(t, func(w http.ResponseWriter, call gqlCall) {
		_, _ = fmt.Fprint(w, `{"errors":[{"message":"internal server error"}]}`)
	})
	c2 := newTestConnector(t, nil, h2)
	_, err = collectScan(c2, source.ScanRequest{
		Table:   "alert_conditions",
		Filters: []source.Filter{{Column: "policy_id", Op: source.OpEq, Value: "42"}},
	})
	require.Error(t, err)
}

func TestScanPolicies(t *testing.T) {
	_, h := capture(t, func(w http.ResponseWriter, call gqlCall) {
		_, _ = fmt.Fprint(w, `{"data":{"actor":{"account":{"alerts":{"policiesSearch":{
			"policies":[{"id":"9","name":"golden","incidentPreference":"PER_CONDITION"}],"nextCursor":null}}}}}}`)
	})
	c := newTestConnector(t, nil, h)

	rows, err := collectScan(c, source.ScanRequest{Table: "alert_policies"})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	assert.Equal(t, []any{"9", "golden", "PER_CONDITION", int64(1)}, rows.Rows[0])
}

// aiIssues fields are documented inconsistently (string vs [String]); both
// shapes must decode.
func TestScanIssuesFlexibleShapes(t *testing.T) {
	_, h := capture(t, func(w http.ResponseWriter, call gqlCall) {
		_, _ = fmt.Fprint(w, `{"data":{"actor":{"account":{"aiIssues":{"issues":{
			"issues":[
			  {"issueId":"i1","title":["High CPU"],"priority":"CRITICAL","state":["ACTIVATED"],
			   "createdAt":1750000000000,"closedAt":null,"entityNames":["web"],"entityTypes":"APPLICATION"},
			  {"issueId":"i2","title":"Plain title","priority":["HIGH"],"state":"CLOSED",
			   "createdAt":1750000001000,"closedAt":1750000002000,"entityNames":[],"entityTypes":[]},
			  {"issueId":"i3","title":"No entities","priority":"LOW","state":"CLOSED",
			   "createdAt":1750000003000,"closedAt":null,"entityNames":null,"entityTypes":null}
			],
			"nextCursor":null}}}}}}`)
	})
	c := newTestConnector(t, nil, h)

	rows, err := collectScan(c, source.ScanRequest{Table: "issues"})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 3)

	assert.Equal(t, "High CPU", rows.Rows[0][1])  // array title -> first element
	assert.Equal(t, "ACTIVATED", rows.Rows[0][3]) // array state
	assert.Equal(t, int64(1750000000000), rows.Rows[0][4])
	assert.Nil(t, rows.Rows[0][5]) // null closedAt
	assert.JSONEq(t, `["web"]`, rows.Rows[0][6].(string))
	assert.JSONEq(t, `["APPLICATION"]`, rows.Rows[0][7].(string)) // string -> array

	assert.Equal(t, "Plain title", rows.Rows[1][1]) // plain string title
	assert.Equal(t, int64(1750000002000), rows.Rows[1][5])
	assert.JSONEq(t, `[]`, rows.Rows[1][6].(string)) // empty array stays "[]"

	assert.Nil(t, rows.Rows[2][6]) // JSON null -> SQL NULL, not the text "null"
	assert.Nil(t, rows.Rows[2][7])
}

func TestScanIncidents(t *testing.T) {
	_, h := capture(t, func(w http.ResponseWriter, call gqlCall) {
		_, _ = fmt.Fprint(w, `{"data":{"actor":{"account":{"aiIssues":{"incidents":{
			"incidents":[{"incidentId":"n1","title":"disk full","description":["p full on host"],
			              "priority":"CRITICAL","state":"CREATED","createdAt":1,"closedAt":null,"entityGuids":"G1"}],
			"nextCursor":null}}}}}}`)
	})
	c := newTestConnector(t, nil, h)

	rows, err := collectScan(c, source.ScanRequest{Table: "incidents"})
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	assert.Equal(t, "disk full", rows.Rows[0][1])
	assert.Equal(t, "p full on host", rows.Rows[0][2])
	assert.JSONEq(t, `["G1"]`, rows.Rows[0][7].(string))
}

func TestTablesListsCurated(t *testing.T) {
	t.Setenv("NEW_RELIC_API_KEY", "k")
	c, err := New(map[string]any{"account_id": 1})
	require.NoError(t, err)
	tables := c.Tables()
	names := make([]string, 0, len(tables))
	for _, ts := range tables {
		names = append(names, ts.Name)
		assert.NotEmpty(t, ts.Columns)
	}
	assert.ElementsMatch(t,
		[]string{"accounts", "entities", "alert_policies", "alert_conditions", "issues", "incidents"}, names)
}
