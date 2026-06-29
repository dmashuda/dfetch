package discord

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"

	"github.com/dmashuda/dfetch/internal/source"
	"github.com/dmashuda/dfetch/internal/sqlparse"
)

func col(name, typ string) source.Column { return source.Column{Name: name, Type: typ} }

// Column names mirror the Discord API JSON fields. snowflake ids are TEXT (they
// exceed JS/SQLite-safe integers); roles/reactions are kept as JSON strings
// queryable with SQLite's json_extract.
var channelsCols = []source.Column{
	col("guild_id", "TEXT"), col("id", "TEXT"), col("name", "TEXT"), col("type", "INTEGER"),
	col("position", "INTEGER"), col("topic", "TEXT"), col("nsfw", "INTEGER"),
	col("parent_id", "TEXT"), col("rate_limit_per_user", "INTEGER"),
}

var membersCols = []source.Column{
	col("guild_id", "TEXT"), col("user_id", "TEXT"), col("username", "TEXT"),
	col("global_name", "TEXT"), col("nick", "TEXT"), col("is_bot", "INTEGER"),
	col("joined_at", "TEXT"), col("premium_since", "TEXT"), col("roles", "TEXT"),
}

var messagesCols = []source.Column{
	col("channel_id", "TEXT"), col("id", "TEXT"), col("author_id", "TEXT"),
	col("author_username", "TEXT"), col("content", "TEXT"), col("timestamp", "TEXT"),
	col("edited_timestamp", "TEXT"), col("type", "INTEGER"), col("pinned", "INTEGER"),
	col("reactions", "TEXT"),
}

var threadsCols = []source.Column{
	col("guild_id", "TEXT"), col("id", "TEXT"), col("name", "TEXT"), col("type", "INTEGER"),
	col("parent_id", "TEXT"), col("owner_id", "TEXT"), col("message_count", "INTEGER"),
	col("member_count", "INTEGER"), col("archived", "INTEGER"), col("locked", "INTEGER"),
	col("auto_archive_duration", "INTEGER"),
}

// --- JSON shapes ---

type dcUser struct {
	ID         string  `json:"id"`
	Username   string  `json:"username"`
	GlobalName *string `json:"global_name"`
	Bot        bool    `json:"bot"`
}

type dcChannel struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	Type             int64   `json:"type"`
	Position         int64   `json:"position"`
	Topic            *string `json:"topic"`
	NSFW             bool    `json:"nsfw"`
	ParentID         *string `json:"parent_id"`
	RateLimitPerUser int64   `json:"rate_limit_per_user"`
}

type dcMember struct {
	User         dcUser   `json:"user"`
	Nick         *string  `json:"nick"`
	Roles        []string `json:"roles"`
	JoinedAt     string   `json:"joined_at"`
	PremiumSince *string  `json:"premium_since"`
}

type dcMessage struct {
	ID              string          `json:"id"`
	Author          dcUser          `json:"author"`
	Content         string          `json:"content"`
	Timestamp       string          `json:"timestamp"`
	EditedTimestamp *string         `json:"edited_timestamp"`
	Type            int64           `json:"type"`
	Pinned          bool            `json:"pinned"`
	Reactions       json.RawMessage `json:"reactions"`
}

type dcThread struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Type           int64   `json:"type"`
	ParentID       *string `json:"parent_id"`
	OwnerID        *string `json:"owner_id"`
	MessageCount   int64   `json:"message_count"`
	MemberCount    int64   `json:"member_count"`
	ThreadMetadata struct {
		Archived            bool  `json:"archived"`
		Locked              bool  `json:"locked"`
		AutoArchiveDuration int64 `json:"auto_archive_duration"`
	} `json:"thread_metadata"`
}

type dcActiveThreads struct {
	Threads []dcThread `json:"threads"`
}

// --- helpers ---

// ptrStr maps a nil string pointer to SQL NULL, else its value.
func ptrStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// rawJSON stores a raw JSON value as a TEXT column, or NULL when absent.
func rawJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// jsonList marshals a string slice to a JSON array string (e.g. role ids).
func jsonList(items []string) any {
	if items == nil {
		return nil
	}
	b, err := json.Marshal(items)
	if err != nil {
		return nil
	}
	return string(b)
}

// asString renders a filter literal (string/int64/float64) for a snowflake id.
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

// scanChannels lists a guild's channels. The endpoint returns every channel in a
// single response (no pagination or limit param), so SQLite applies any LIMIT.
func (c *Connector) scanChannels(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	guild, err := requireStringEq(req, "guild_id")
	if err != nil {
		return err
	}
	var items []dcChannel
	if err := c.get(ctx, "/guilds/"+url.PathEscape(guild)+"/channels", nil, &items); err != nil {
		return err
	}
	rows := make([][]any, len(items))
	for i, ch := range items {
		rows[i] = []any{
			guild, ch.ID, ch.Name, ch.Type, ch.Position, ptrStr(ch.Topic),
			ch.NSFW, ptrStr(ch.ParentID), ch.RateLimitPerUser,
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return emit(&source.Rows{Columns: colNames(channelsCols), Rows: rows})
}

// --- members ---

// scanMembers lists a guild's members. The endpoint paginates ascending by user
// id via the `after` cursor (max 1000 per page); it needs the GUILD_MEMBERS
// privileged intent.
func (c *Connector) scanMembers(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	guild, err := requireStringEq(req, "guild_id")
	if err != nil {
		return err
	}
	q := url.Values{}
	perPage, stopAt, pushLimit := pageLimit(req, limitSafe(req, "guild_id"), 1000)
	q.Set("limit", strconv.Itoa(perPage))

	return cursorPages(ctx, "members", membersCols, stopAt, pushLimit,
		func(ctx context.Context, cursor string) ([][]any, string, error) {
			if cursor != "" {
				q.Set("after", cursor)
			}
			var items []dcMember
			if err := c.get(ctx, "/guilds/"+url.PathEscape(guild)+"/members", q, &items); err != nil {
				return nil, "", err
			}
			rows := make([][]any, len(items))
			for i, m := range items {
				rows[i] = []any{
					guild, m.User.ID, m.User.Username, ptrStr(m.User.GlobalName), ptrStr(m.Nick),
					m.User.Bot, strOrNil(m.JoinedAt), ptrStr(m.PremiumSince), jsonList(m.Roles),
				}
			}
			// Members come back ascending by id; the last is the cursor. A short
			// page is the last page.
			next := ""
			if len(items) == perPage && len(items) > 0 {
				next = items[len(items)-1].User.ID
			}
			return rows, next, nil
		}, emit)
}

// strOrNil maps an empty string to SQL NULL.
func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --- messages ---

// scanMessages lists a channel's messages, newest-first. It paginates older via
// the `before` cursor (max 100 per page) and pushes an exclusive id upper bound
// (id < V) to `before`.
func (c *Connector) scanMessages(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	channel, err := requireStringEq(req, "channel")
	if err != nil {
		return err
	}
	q := url.Values{}
	if before, ok := idUpperBound(req); ok {
		q.Set("before", before)
	}
	perPage, stopAt, pushLimit := pageLimit(req, messagesLimitSafe(req), 100)
	q.Set("limit", strconv.Itoa(perPage))

	return cursorPages(ctx, "messages", messagesCols, stopAt, pushLimit,
		func(ctx context.Context, cursor string) ([][]any, string, error) {
			if cursor != "" {
				q.Set("before", cursor)
			}
			var items []dcMessage
			if err := c.get(ctx, "/channels/"+url.PathEscape(channel)+"/messages", q, &items); err != nil {
				return nil, "", err
			}
			rows := make([][]any, len(items))
			for i, m := range items {
				rows[i] = []any{
					channel, m.ID, m.Author.ID, m.Author.Username, m.Content, m.Timestamp,
					ptrStr(m.EditedTimestamp), m.Type, m.Pinned, rawJSON(m.Reactions),
				}
			}
			// Messages come back newest-first; the last (oldest) id is the cursor
			// to page further back. A short page is the last page.
			next := ""
			if len(items) == perPage && len(items) > 0 {
				next = items[len(items)-1].ID
			}
			return rows, next, nil
		}, emit)
}

// idUpperBound returns the `before` value from an exclusive id upper bound
// (id < V). before is exclusive in Discord, matching `<` exactly; inclusive or
// lower bounds are left to SQLite (and don't push LIMIT).
func idUpperBound(req source.ScanRequest) (string, bool) {
	for _, f := range req.Filters {
		if f.Column == "id" && f.Op == sqlparse.OpLt {
			if s := asString(f.Value); s != "" {
				return s, true
			}
		}
	}
	return "", false
}

// messagesLimitSafe reports whether LIMIT can be pushed to the messages endpoint.
// It returns newest-first, so a pushed LIMIT is exact only when every filter is
// consumed and the order matches the API's. Allowed: channel equality, and id
// bounds that never reduce the newest-N set — an exclusive id < V (sent as the
// exact `before`) or a lower bound id > / >= V (which only trims the oldest tail,
// reapplied by SQLite). An inclusive id <= V or BETWEEN would drop the boundary
// row and is left to SQLite (no LIMIT push).
func messagesLimitSafe(req source.ScanRequest) bool {
	for _, f := range req.Filters {
		switch f.Column {
		case "channel":
			if f.Op != sqlparse.OpEq {
				return false
			}
		case "id":
			switch f.Op {
			case sqlparse.OpLt, sqlparse.OpGt, sqlparse.OpGte:
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
		c := req.OrderBy[0]
		return (c.Column == "id" || c.Column == "timestamp") && c.Desc
	default:
		return false
	}
}

// --- threads ---

// scanThreads lists a guild's active threads. The endpoint returns them all in a
// single response, so SQLite applies any LIMIT.
func (c *Connector) scanThreads(ctx context.Context, req source.ScanRequest, emit func(*source.Rows) error) error {
	guild, err := requireStringEq(req, "guild_id")
	if err != nil {
		return err
	}
	var resp dcActiveThreads
	if err := c.get(ctx, "/guilds/"+url.PathEscape(guild)+"/threads/active", nil, &resp); err != nil {
		return err
	}
	rows := make([][]any, len(resp.Threads))
	for i, t := range resp.Threads {
		rows[i] = []any{
			guild, t.ID, t.Name, t.Type, ptrStr(t.ParentID), ptrStr(t.OwnerID),
			t.MessageCount, t.MemberCount, t.ThreadMetadata.Archived, t.ThreadMetadata.Locked,
			t.ThreadMetadata.AutoArchiveDuration,
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return emit(&source.Rows{Columns: colNames(threadsCols), Rows: rows})
}
