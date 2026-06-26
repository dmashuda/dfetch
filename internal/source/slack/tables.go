package slack

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/sqlparse"
)

func col(name, typ string) source.Column { return source.Column{Name: name, Type: typ} }

// Column names mirror the Slack API JSON fields where practical. Nested objects
// (topic/purpose) are flattened to their text value; reactions are kept as a JSON
// string queryable with SQLite's json_extract.
var channelsCols = []source.Column{
	col("id", "TEXT"), col("name", "TEXT"), col("is_private", "INTEGER"),
	col("is_archived", "INTEGER"), col("is_general", "INTEGER"),
	col("num_members", "INTEGER"), col("topic", "TEXT"), col("purpose", "TEXT"),
	col("creator", "TEXT"), col("created", "INTEGER"),
}

var usersCols = []source.Column{
	col("id", "TEXT"), col("name", "TEXT"), col("real_name", "TEXT"),
	col("display_name", "TEXT"), col("email", "TEXT"), col("is_bot", "INTEGER"),
	col("is_admin", "INTEGER"), col("deleted", "INTEGER"), col("tz", "TEXT"),
	col("title", "TEXT"),
}

var messagesCols = []source.Column{
	col("channel", "TEXT"), col("ts", "TEXT"), col("thread_ts", "TEXT"),
	col("user", "TEXT"), col("type", "TEXT"), col("subtype", "TEXT"),
	col("text", "TEXT"), col("reply_count", "INTEGER"), col("reactions", "TEXT"),
	col("edited_ts", "TEXT"),
}

var searchCols = []source.Column{
	col("query", "TEXT"), col("ts", "TEXT"), col("channel_id", "TEXT"),
	col("channel_name", "TEXT"), col("user", "TEXT"), col("username", "TEXT"),
	col("text", "TEXT"), col("permalink", "TEXT"), col("score", "REAL"),
}

// --- JSON shapes ---

type textObj struct {
	Value string `json:"value"`
}

type slackChannel struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	IsPrivate  bool    `json:"is_private"`
	IsArchived bool    `json:"is_archived"`
	IsGeneral  bool    `json:"is_general"`
	NumMembers int64   `json:"num_members"`
	Creator    string  `json:"creator"`
	Created    int64   `json:"created"`
	Topic      textObj `json:"topic"`
	Purpose    textObj `json:"purpose"`
}

type channelsResponse struct {
	Channels []slackChannel `json:"channels"`
}

type slackUser struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RealName string `json:"real_name"`
	Deleted  bool   `json:"deleted"`
	IsBot    bool   `json:"is_bot"`
	IsAdmin  bool   `json:"is_admin"`
	TZ       string `json:"tz"`
	Profile  struct {
		RealName    string `json:"real_name"`
		DisplayName string `json:"display_name"`
		Email       string `json:"email"`
		Title       string `json:"title"`
	} `json:"profile"`
}

type usersResponse struct {
	Members []slackUser `json:"members"`
}

type slackMessage struct {
	Type       string          `json:"type"`
	Subtype    string          `json:"subtype"`
	Ts         string          `json:"ts"`
	User       string          `json:"user"`
	Text       string          `json:"text"`
	ThreadTs   string          `json:"thread_ts"`
	ReplyCount int64           `json:"reply_count"`
	Reactions  json.RawMessage `json:"reactions"`
	Edited     *struct {
		Ts string `json:"ts"`
	} `json:"edited"`
}

type messagesResponse struct {
	Messages []slackMessage `json:"messages"`
}

type searchMatch struct {
	Type     string `json:"type"`
	Ts       string `json:"ts"`
	User     string `json:"user"`
	Username string `json:"username"`
	Text     string `json:"text"`
	Channel  struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"channel"`
	Permalink string  `json:"permalink"`
	Score     float64 `json:"score"`
}

type searchResponse struct {
	Messages struct {
		Matches []searchMatch `json:"matches"`
		Paging  struct {
			Count int `json:"count"`
			Total int `json:"total"`
			Page  int `json:"page"`
			Pages int `json:"pages"`
		} `json:"paging"`
	} `json:"messages"`
}

// --- helpers ---

// strOrNil maps an empty string to SQL NULL, so optional fields read as NULL
// rather than "".
func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// rawJSON stores a raw JSON value as a TEXT column, or NULL when absent.
func rawJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// asString renders a filter literal (string/int64/float64) as the string Slack
// expects for ts bounds (a Unix timestamp, possibly with a fractional part).
func asString(v any) string {
	switch n := v.(type) {
	case string:
		return n
	case int64:
		return strconv.FormatInt(n, 10)
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	default:
		return ""
	}
}

// --- channels ---

func (c *Connector) scanChannels(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	q := url.Values{}
	q.Set("types", "public_channel,private_channel")
	// is_archived = 0 maps to exclude_archived; SQLite still re-applies the filter.
	if n, ok := intEq(req, "is_archived"); ok && n == 0 {
		q.Set("exclude_archived", "true")
	}
	perPage, stopAt, pushLimit := pageLimit(req, limitSafe(req))
	q.Set("limit", strconv.Itoa(perPage))

	return cursorPages(ctx, "channels", channelsCols, stopAt, pushLimit,
		func(ctx context.Context, cursor string) ([][]any, string, error) {
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			var resp channelsResponse
			next, err := c.get(ctx, "conversations.list", q, &resp)
			if err != nil {
				return nil, "", err
			}
			rows := make([][]any, len(resp.Channels))
			for i, ch := range resp.Channels {
				rows[i] = []any{
					ch.ID, ch.Name, ch.IsPrivate, ch.IsArchived, ch.IsGeneral,
					ch.NumMembers, ch.Topic.Value, ch.Purpose.Value, strOrNil(ch.Creator), ch.Created,
				}
			}
			return rows, next, nil
		}, emit)
}

// --- users ---

func (c *Connector) scanUsers(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	q := url.Values{}
	perPage, stopAt, pushLimit := pageLimit(req, limitSafe(req))
	q.Set("limit", strconv.Itoa(perPage))

	return cursorPages(ctx, "users", usersCols, stopAt, pushLimit,
		func(ctx context.Context, cursor string) ([][]any, string, error) {
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			var resp usersResponse
			next, err := c.get(ctx, "users.list", q, &resp)
			if err != nil {
				return nil, "", err
			}
			rows := make([][]any, len(resp.Members))
			for i, u := range resp.Members {
				realName := u.Profile.RealName
				if realName == "" {
					realName = u.RealName
				}
				rows[i] = []any{
					u.ID, u.Name, strOrNil(realName), strOrNil(u.Profile.DisplayName),
					strOrNil(u.Profile.Email), u.IsBot, u.IsAdmin, u.Deleted,
					strOrNil(u.TZ), strOrNil(u.Profile.Title),
				}
			}
			return rows, next, nil
		}, emit)
}

// --- messages ---

func (c *Connector) scanMessages(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	channel, err := requireStringEq(req, "channel")
	if err != nil {
		return err
	}

	q := url.Values{}
	q.Set("channel", channel)
	// A ts range narrows the history window via oldest/latest, sent as an
	// inclusive window (Slack excludes the bounds by default) so the API returns a
	// superset; SQLite re-applies the exact ts operator verbatim.
	oldest, latest := tsBounds(req)
	if oldest != "" {
		q.Set("oldest", oldest)
	}
	if latest != "" {
		q.Set("latest", latest)
	}
	if oldest != "" || latest != "" {
		q.Set("inclusive", "true")
	}
	perPage, stopAt, pushLimit := pageLimit(req, messagesLimitSafe(req))
	q.Set("limit", strconv.Itoa(perPage))

	return cursorPages(ctx, "messages", messagesCols, stopAt, pushLimit,
		func(ctx context.Context, cursor string) ([][]any, string, error) {
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			var resp messagesResponse
			next, err := c.get(ctx, "conversations.history", q, &resp)
			if err != nil {
				return nil, "", err
			}
			rows := make([][]any, len(resp.Messages))
			for i, m := range resp.Messages {
				var editedTs any
				if m.Edited != nil {
					editedTs = m.Edited.Ts
				}
				rows[i] = []any{
					channel, m.Ts, strOrNil(m.ThreadTs), strOrNil(m.User), m.Type,
					strOrNil(m.Subtype), m.Text, m.ReplyCount, rawJSON(m.Reactions), editedTs,
				}
			}
			return rows, next, nil
		}, emit)
}

// tsBounds derives Slack oldest/latest bounds from the ts range filters. It
// combines every ts conjunct (e.g. ts > A AND ts < B) so both ends of a window
// are pushed: req.Filter returns only the first match, which would silently drop
// the second bound and, with a pushed LIMIT, return too few rows.
func tsBounds(req source.ScanRequest) (oldest, latest string) {
	for _, f := range req.Filters {
		if f.Column != "ts" {
			continue
		}
		switch f.Op {
		case sqlparse.OpGt, sqlparse.OpGte:
			oldest = asString(f.Value)
		case sqlparse.OpLt, sqlparse.OpLte:
			latest = asString(f.Value)
		case sqlparse.OpBetween:
			if len(f.Values) == 2 {
				oldest = asString(f.Values[0])
				latest = asString(f.Values[1])
			}
		}
	}
	return oldest, latest
}

// messagesLimitSafe reports whether LIMIT can be pushed to conversations.history.
// The endpoint returns messages newest-first, so a LIMIT is exact only when every
// filter is consumed (channel equality, ts range → oldest/latest) and the order
// is either absent or a single ts DESC (matching the API's order). Only inclusive
// ts bounds (>=, <=, BETWEEN) qualify: an exclusive >/< maps to Slack's inclusive
// window, so the API would return a boundary row SQLite then drops, undercounting
// a pushed LIMIT. Such ranges still work — they just let SQLite apply the LIMIT.
func messagesLimitSafe(req source.ScanRequest) bool {
	for _, f := range req.Filters {
		switch f.Column {
		case "channel":
			if f.Op != sqlparse.OpEq {
				return false
			}
		case "ts":
			switch f.Op {
			case sqlparse.OpGte, sqlparse.OpLte, sqlparse.OpBetween:
			default:
				return false
			}
		default:
			return false
		}
	}
	switch len(req.OrderBy) {
	case 0:
		return true
	case 1:
		return req.OrderBy[0].Column == "ts" && req.OrderBy[0].Desc
	default:
		return false
	}
}

// --- search ---

func (c *Connector) scanSearch(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	query, err := requireStringEq(req, "query")
	if err != nil {
		return err
	}

	// search.messages is page-based (not cursor-based) and tops out at count=100.
	perPage, stopAt, pushLimit := pageLimit(req, searchLimitSafe(req))
	if perPage > 100 {
		perPage = 100
	}

	q := url.Values{}
	q.Set("query", query)
	q.Set("count", strconv.Itoa(perPage))

	sent := 0
	for page := 1; ; page++ {
		q.Set("page", strconv.Itoa(page))
		var resp searchResponse
		if _, err := c.get(ctx, "search.messages", q, &resp); err != nil {
			return err
		}
		rows := make([][]any, 0, len(resp.Messages.Matches))
		for _, m := range resp.Messages.Matches {
			rows = append(rows, []any{
				query, m.Ts, m.Channel.ID, strOrNil(m.Channel.Name), strOrNil(m.User),
				strOrNil(m.Username), m.Text, strOrNil(m.Permalink), m.Score,
			})
			sent++
			if pushLimit && sent >= stopAt {
				break
			}
		}
		if len(rows) > 0 {
			if err := emit(&source.Rows{Columns: colNames(searchCols), Rows: rows}); err != nil {
				return err
			}
		}
		if pushLimit && sent >= stopAt {
			return nil
		}
		pages := resp.Messages.Paging.Pages
		if pages == 0 || page >= pages {
			return nil
		}
		if !pushLimit && page >= maxPages {
			return emit(source.Warn("slack.search: stopped at the %d-page cap; results may be incomplete — add a LIMIT or a narrower query", maxPages))
		}
	}
}

// searchLimitSafe reports whether LIMIT can be pushed to search.messages: only
// when the sole filter is the query and there is no ORDER BY (the API returns
// results by relevance, which an ORDER BY would re-sort).
func searchLimitSafe(req source.ScanRequest) bool {
	if len(req.OrderBy) != 0 {
		return false
	}
	for _, f := range req.Filters {
		if f.Column != "query" || f.Op != sqlparse.OpEq {
			return false
		}
	}
	return true
}
