package sqlparse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseErrorDetails asserts that rejected queries report a precise location
// and an explanatory reason, not just "invalid".
func TestParseErrorDetails(t *testing.T) {
	cases := []struct {
		name        string
		sql         string
		wantPos     *Position // nil means "no position expected"
		msgContains string
	}{
		{
			name:        "multiple statements points at second statement",
			sql:         "SELECT 1; SELECT 2",
			wantPos:     &Position{Line: 1, Column: 10},
			msgContains: "single statement",
		},
		{
			name:        "explain points at the EXPLAIN keyword",
			sql:         "EXPLAIN SELECT * FROM t",
			wantPos:     &Position{Line: 1, Column: 0},
			msgContains: "EXPLAIN is not supported",
		},
		{
			name:        "insert points at the statement start",
			sql:         "INSERT INTO t VALUES (1)",
			wantPos:     &Position{Line: 1, Column: 0},
			msgContains: "read-only SELECT",
		},
		{
			name:        "update points at the statement start",
			sql:         "UPDATE t SET x = 1",
			wantPos:     &Position{Line: 1, Column: 0},
			msgContains: "read-only SELECT",
		},
		{
			name:        "syntax error pinpoints the offending token",
			sql:         "SELECT FROM",
			wantPos:     &Position{Line: 1, Column: 7},
			msgContains: "FROM",
		},
		{
			name:        "syntax error on a later line reports the line",
			sql:         "SELECT *\nFROM",
			wantPos:     &Position{Line: 2, Column: 4},
			msgContains: "",
		},
		{
			name:        "empty query has a reason but no position",
			sql:         "",
			wantPos:     nil,
			msgContains: "no SQL statement",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := Parse(tc.sql)
			require.Error(t, err)
			require.Nil(t, q)

			var perr *Error
			require.ErrorAs(t, err, &perr)
			assert.Contains(t, perr.Msg, tc.msgContains)

			if tc.wantPos == nil {
				assert.Nil(t, perr.Pos)
			} else {
				require.NotNil(t, perr.Pos)
				assert.Equal(t, *tc.wantPos, *perr.Pos)
			}
		})
	}
}

// TestErrorString covers the human-facing formatting of Error.
func TestErrorString(t *testing.T) {
	withPos := &Error{Pos: &Position{Line: 2, Column: 5}, Msg: "boom"}
	assert.Equal(t, "line 2:5: boom", withPos.Error())

	noPos := &Error{Msg: "boom"}
	assert.Equal(t, "boom", noPos.Error())
}
