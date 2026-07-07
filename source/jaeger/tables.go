package jaeger

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"time"

	"github.com/dmashuda/dfetch/source"
)

func col(name, typ string) source.Column { return source.Column{Name: name, Type: typ} }

// Column names follow the OTLP span model, lowercased/underscored, so queries
// read close to the data: operation_name is the span's name, service_name is
// hoisted from the resource's service.name attribute, kind/status_code are the
// OTLP enums rendered as readable strings, and attributes is the span's typed
// attribute list flattened to a JSON object (query it with SQLite's json_extract).
var spansCols = []source.Column{
	col("trace_id", "TEXT"), col("span_id", "TEXT"), col("parent_span_id", "TEXT"),
	col("operation_name", "TEXT"), col("service_name", "TEXT"), col("scope_name", "TEXT"),
	col("kind", "TEXT"),
	col("start_time", "TEXT"), col("start_time_unix_nano", "INTEGER"),
	col("duration_ms", "REAL"),
	col("status_code", "TEXT"), col("status_message", "TEXT"),
	col("attributes", "TEXT"),
}

var servicesCols = []source.Column{col("name", "TEXT")}

var operationsCols = []source.Column{
	col("service_name", "TEXT"), col("name", "TEXT"), col("span_kind", "TEXT"),
}

// --- OTLP JSON shapes (opentelemetry.proto.trace.v1.TracesData) ---

type otlpTraces struct {
	ResourceSpans []resourceSpans `json:"resourceSpans"`
}

type resourceSpans struct {
	Resource   resource     `json:"resource"`
	ScopeSpans []scopeSpans `json:"scopeSpans"`
}

type resource struct {
	Attributes []keyValue `json:"attributes"`
}

type scopeSpans struct {
	Scope scope  `json:"scope"`
	Spans []span `json:"spans"`
}

type scope struct {
	Name string `json:"name"`
}

type span struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	ParentSpanID      string     `json:"parentSpanId"`
	Name              string     `json:"name"`
	Kind              int        `json:"kind"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []keyValue `json:"attributes"`
	Status            status     `json:"status"`
}

type status struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

// anyValue is the OTLP AnyValue oneof; exactly one pointer is non-nil.
type anyValue struct {
	StringValue *string      `json:"stringValue"`
	BoolValue   *bool        `json:"boolValue"`
	IntValue    *string      `json:"intValue"` // OTLP JSON encodes int64 as a string
	DoubleValue *float64     `json:"doubleValue"`
	ArrayValue  *arrayValue  `json:"arrayValue"`
	KvlistValue *kvlistValue `json:"kvlistValue"`
	BytesValue  *string      `json:"bytesValue"`
}

type arrayValue struct {
	Values []anyValue `json:"values"`
}

type kvlistValue struct {
	Values []keyValue `json:"values"`
}

// --- spans ---

func (c *Connector) scanSpans(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	// A trace_id equality is a direct lookup: GET /api/v3/traces/{id} returns the
	// whole trace by ID, with no service_name or time window required.
	if tid, ok := stringEq(req, "trace_id"); ok {
		return c.streamTraces(ctx, c.baseURL+"/api/v3/traces/"+url.PathEscape(tid), emit)
	}

	svc, err := requireStringEq(req, "service_name")
	if err != nil {
		return err
	}

	q := url.Values{}
	q.Set("query.service_name", svc)
	if op, ok := stringEq(req, "operation_name"); ok {
		q.Set("query.operation_name", op)
	}
	min, max := timeBounds(req, time.Now(), defaultWindow)
	q.Set("query.start_time_min", min.UTC().Format(time.RFC3339Nano))
	q.Set("query.start_time_max", max.UTC().Format(time.RFC3339Nano))
	if dmin, dmax := durationBounds(req); dmin != "" || dmax != "" {
		if dmin != "" {
			q.Set("query.duration_min", dmin)
		}
		if dmax != "" {
			q.Set("query.duration_max", dmax)
		}
	}
	// SQL LIMIT is not pushed: search_depth caps traces, but a LIMIT on spans
	// counts spans (one trace has many), so pushing it would be unsound — SQLite
	// applies the LIMIT. search_depth is only a fetch cap on an unbounded scan, and
	// is sent ONLY when max_traces is configured: api_v3 rejects any value not
	// strictly below the server's max-traces limit, so by default we omit it and
	// let the server (plus the time window) bound the scan.
	if c.maxTraces > 0 {
		q.Set("query.search_depth", strconv.Itoa(c.maxTraces))
	}

	// Count distinct traces returned so we can warn if the result looks truncated.
	traces := map[string]struct{}{}
	counting := func(chunk *source.Rows) error {
		for _, row := range chunk.Rows {
			if len(row) > 0 {
				if id, ok := row[0].(string); ok { // spansCols[0] is trace_id
					traces[id] = struct{}{}
				}
			}
		}
		return emit(chunk)
	}
	if err := c.streamTraces(ctx, c.baseURL+"/api/v3/traces?"+q.Encode(), counting); err != nil {
		return err
	}
	if !hasStartTimeLowerBound(req) {
		if err := emit(source.Warn("jaeger.spans: searched only a default %s time window (no lower start_time bound) — add a start_time >= filter to widen", defaultWindow)); err != nil {
			return err
		}
	}
	// Warn when the result is large enough to likely be truncated — by our own cap
	// (max_traces) if set, otherwise by the server's limit (the warning threshold).
	warnAt := c.maxTraces
	if warnAt == 0 {
		warnAt = defaultWarnTraces
	}
	if len(traces) >= warnAt {
		if err := emit(source.Warn("jaeger.spans: returned %d+ traces; results may be truncated — narrow the time range, add filters, or set the max_traces param", warnAt)); err != nil {
			return err
		}
	}
	return nil
}

// streamTraces fetches an api_v3 traces URL and emits one chunk of span rows per
// decoded TracesData. The grpc-gateway streams server responses as one-or-more
// concatenated {"result": TracesData} JSON objects, so it decodes in a loop.
func (c *Connector) streamTraces(ctx context.Context, rawurl string, emit func(*source.Rows) error) error {
	resp, err := c.get(ctx, rawurl)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	dec := json.NewDecoder(resp.Body)
	for {
		var chunk struct {
			Result otlpTraces `json:"result"`
		}
		if err := dec.Decode(&chunk); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		rows := spanRows(chunk.Result)
		if len(rows) > 0 {
			if err := emit(&source.Rows{Columns: colNames(spansCols), Rows: rows}); err != nil {
				return err
			}
		}
	}
}

func spanRows(td otlpTraces) [][]any {
	var rows [][]any
	for _, rs := range td.ResourceSpans {
		svc := attrString(rs.Resource.Attributes, "service.name")
		for _, ss := range rs.ScopeSpans {
			scopeName := ss.Scope.Name
			for _, sp := range ss.Spans {
				start := parseNanos(sp.StartTimeUnixNano)
				end := parseNanos(sp.EndTimeUnixNano)
				rows = append(rows, []any{
					sp.TraceID, sp.SpanID, nullableStr(sp.ParentSpanID),
					sp.Name, svc, nullableStr(scopeName),
					spanKind(sp.Kind),
					startTimeText(start), start,
					durationMillis(start, end),
					statusCodeText(sp.Status.Code), nullableStr(sp.Status.Message),
					attrsJSON(sp.Attributes),
				})
			}
		}
	}
	return rows
}

// --- services ---

func (c *Connector) scanServices(ctx context.Context, _ source.ScanRequest, emit func(*source.Rows) error) error {
	var resp struct {
		Services []string `json:"services"`
	}
	if err := c.getJSON(ctx, c.baseURL+"/api/v3/services", &resp); err != nil {
		return err
	}
	rows := make([][]any, 0, len(resp.Services))
	for _, s := range resp.Services {
		rows = append(rows, []any{s})
	}
	if len(rows) == 0 {
		return nil
	}
	return emit(&source.Rows{Columns: colNames(servicesCols), Rows: rows})
}

// --- operations ---

func (c *Connector) scanOperations(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	svc, err := requireStringEq(req, "service_name")
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("service", svc)
	if k, ok := stringEq(req, "span_kind"); ok {
		q.Set("spanKind", k)
	}

	var resp struct {
		Operations []struct {
			Name     string `json:"name"`
			SpanKind string `json:"spanKind"`
		} `json:"operations"`
	}
	if err := c.getJSON(ctx, c.baseURL+"/api/v3/operations?"+q.Encode(), &resp); err != nil {
		return err
	}
	rows := make([][]any, 0, len(resp.Operations))
	for _, o := range resp.Operations {
		rows = append(rows, []any{svc, o.Name, o.SpanKind})
	}
	if len(rows) == 0 {
		return nil
	}
	return emit(&source.Rows{Columns: colNames(operationsCols), Rows: rows})
}

// --- row helpers ---

var spanKinds = map[int]string{
	0: "unspecified", 1: "internal", 2: "server", 3: "client", 4: "producer", 5: "consumer",
}

func spanKind(k int) string {
	if s, ok := spanKinds[k]; ok {
		return s
	}
	return "unspecified"
}

var statusCodes = map[int]string{0: "unset", 1: "ok", 2: "error"}

func statusCodeText(c int) string {
	if s, ok := statusCodes[c]; ok {
		return s
	}
	return "unset"
}

func parseNanos(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// startTimeText renders a span start as fixed-width UTC RFC3339 text (nine
// zero-padded fractional digits): with a constant offset and constant width,
// lexical order over the column matches chronological order, so SQLite's
// verbatim ORDER BY / range re-filters over the text behave chronologically.
// (RFC3339Nano trims trailing zeros, which breaks this: "…05Z" sorts after
// "…05.5Z" because 'Z' > '.'.)
func startTimeText(nanos int64) any {
	if nanos == 0 {
		return nil
	}
	return time.Unix(0, nanos).UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func durationMillis(start, end int64) any {
	if start == 0 || end == 0 {
		return nil
	}
	return float64(end-start) / 1e6
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// attrString returns the string value of the named resource/span attribute, or
// nil when absent.
func attrString(kvs []keyValue, key string) any {
	for _, kv := range kvs {
		if kv.Key == key && kv.Value.StringValue != nil {
			return *kv.Value.StringValue
		}
	}
	return nil
}

// attrsJSON flattens an OTLP attribute list to a JSON object string (always a
// valid object, "{}" when empty) so it's queryable via SQLite's json functions.
func attrsJSON(kvs []keyValue) any {
	m := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		m[kv.Key] = anyVal(kv.Value)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// anyVal extracts the Go value from an OTLP AnyValue, recursing into arrays and
// key/value lists.
func anyVal(v anyValue) any {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.BoolValue != nil:
		return *v.BoolValue
	case v.IntValue != nil:
		if n, err := strconv.ParseInt(*v.IntValue, 10, 64); err == nil {
			return n
		}
		return *v.IntValue
	case v.DoubleValue != nil:
		return *v.DoubleValue
	case v.BytesValue != nil:
		return *v.BytesValue
	case v.ArrayValue != nil:
		out := make([]any, 0, len(v.ArrayValue.Values))
		for _, e := range v.ArrayValue.Values {
			out = append(out, anyVal(e))
		}
		return out
	case v.KvlistValue != nil:
		m := make(map[string]any, len(v.KvlistValue.Values))
		for _, kv := range v.KvlistValue.Values {
			m[kv.Key] = anyVal(kv.Value)
		}
		return m
	default:
		return nil
	}
}

func colNames(cols []source.Column) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}
