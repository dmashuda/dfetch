package source

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustCredential(t *testing.T, name string, params map[string]any, staticKey string, env func() string) *Credential {
	t.Helper()
	c, err := NewCredential("test", name, params, staticKey, env)
	require.NoError(t, err)
	return c
}

// The full precedence chain: static param > env > func > command, each source
// shadowing everything below it.
func TestCredentialPrecedence(t *testing.T) {
	ctx := context.Background()
	fn := func(context.Context) (string, error) { return "from-func", nil }
	params := map[string]any{
		"token":         "from-static",
		"token_func":    fn,
		"token_command": []any{"printf", "from-command"},
	}
	env := func() string { return "from-env" }

	c := mustCredential(t, "token", params, "token", env)
	v, err := c.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, "from-static", v)

	// Without the static param, env wins.
	delete(params, "token")
	c = mustCredential(t, "token", params, "token", env)
	v, err = c.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, "from-env", v)

	// Env unset: the func wins over the command.
	c = mustCredential(t, "token", params, "token", func() string { return "" })
	v, err = c.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, "from-func", v)

	// Only the command left.
	delete(params, "token_func")
	c = mustCredential(t, "token", params, "token", nil)
	v, err = c.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, "from-command", v)

	// Nothing configured: empty value, nil error.
	c = mustCredential(t, "token", map[string]any{}, "token", nil)
	v, err = c.Get(ctx)
	require.NoError(t, err)
	assert.Empty(t, v)
}

// A bare func(context.Context) (string, error) is accepted without converting
// to CredentialFunc (an alias, so both spellings are the same type).
func TestCredentialFuncPlainType(t *testing.T) {
	params := map[string]any{
		"token_func": func(context.Context) (string, error) { return "plain", nil },
	}
	c := mustCredential(t, "token", params, "", nil)
	v, err := c.Get(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "plain", v)
}

func TestCredentialFuncWrongType(t *testing.T) {
	_, err := NewCredential("github", "token", map[string]any{"token_func": "not-a-func"}, "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "github: token_func must be a func(context.Context) (string, error)")
}

func TestCredentialCommandParseErrors(t *testing.T) {
	_, err := NewCredential("jira", "auth_header", map[string]any{"auth_header_command": "not-a-list"}, "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "jira: auth_header_command must be a list of strings")

	_, err = NewCredential("jira", "auth_header", map[string]any{"auth_header_command": []any{"echo", 42}}, "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth_header_command[1] must be a string")

	_, err = NewCredential("jira", "auth_header", map[string]any{"auth_header_command": []any{"echo", "  "}}, "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth_header_command[1] must not be empty")
}

// An explicitly empty command list means "not configured" (used by tests to
// disable a default command), not an error.
func TestCredentialEmptyCommandList(t *testing.T) {
	c := mustCredential(t, "token", map[string]any{"token_command": []any{}}, "", nil)
	v, err := c.Get(context.Background())
	require.NoError(t, err)
	assert.Empty(t, v)
}

// Nothing is resolved at construction: the func runs on first Get, once, and
// the value is cached.
func TestCredentialLazyAndCached(t *testing.T) {
	var calls atomic.Int32
	params := map[string]any{
		"token_func": func(context.Context) (string, error) {
			calls.Add(1)
			return "v", nil
		},
	}
	c := mustCredential(t, "token", params, "", nil)
	assert.Equal(t, int32(0), calls.Load(), "must not resolve at construction")

	for range 3 {
		v, err := c.Get(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "v", v)
	}
	assert.Equal(t, int32(1), calls.Load(), "must resolve exactly once")
}

// A failed resolution is NOT cached: the next Get retries, so a transient
// failure (or a canceled first query) doesn't poison a long-lived process.
func TestCredentialErrorRetried(t *testing.T) {
	var calls atomic.Int32
	params := map[string]any{
		"token_func": func(context.Context) (string, error) {
			if calls.Add(1) == 1 {
				return "", errors.New("transient secrets-manager blip")
			}
			return "recovered", nil
		},
	}
	c := mustCredential(t, "token", params, "", nil)

	_, err := c.Get(context.Background())
	require.Error(t, err)

	v, err := c.Get(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "recovered", v)

	// Success is cached: no third resolution.
	v, err = c.Get(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "recovered", v)
	assert.Equal(t, int32(2), calls.Load())
}

// A configured func that yields an empty value is an error, not a cached
// empty credential: the next Get retries, so a secrets store that is briefly
// empty doesn't poison the process.
func TestCredentialEmptyFuncRetried(t *testing.T) {
	var calls atomic.Int32
	params := map[string]any{
		"token_func": func(context.Context) (string, error) {
			if calls.Add(1) == 1 {
				return "", nil // store not yet populated
			}
			return "populated", nil
		},
	}
	c := mustCredential(t, "token", params, "", nil)

	_, err := c.Get(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token_func returned an empty value")

	v, err := c.Get(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "populated", v)
}

// Likewise for a command that produces no output.
func TestCredentialEmptyCommandOutput(t *testing.T) {
	c := mustCredential(t, "token", map[string]any{"token_command": []any{"true"}}, "", nil)
	_, err := c.Get(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token_command")
	assert.Contains(t, err.Error(), "produced no output")
}

// CRLF-terminated command output (e.g. from a Windows-built helper) must not
// leave a trailing \r — net/http rejects header values containing one.
func TestCredentialCommandTrimsCRLF(t *testing.T) {
	c := mustCredential(t, "token",
		map[string]any{"token_command": []any{"printf", `Bearer tok\r\n`}}, "", nil)
	v, err := c.Get(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Bearer tok", v)
}

// A cancellation inherited from the first caller's ctx (e.g. a sibling scan's
// error canceling the errgroup) doesn't latch: the next query resolves fine.
func TestCredentialCanceledFirstCallRetries(t *testing.T) {
	params := map[string]any{
		"token_func": func(ctx context.Context) (string, error) {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			return "ok", nil
		},
	}
	c := mustCredential(t, "token", params, "", nil)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Get(canceled)
	require.Error(t, err)

	v, err := c.Get(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ok", v)
}

// The env closure is consulted at Get time, not at construction.
func TestCredentialEnvReadLazily(t *testing.T) {
	t.Setenv("DFETCH_TEST_CRED", "")
	c := mustCredential(t, "token", map[string]any{}, "", EnvFirst("DFETCH_TEST_CRED"))
	t.Setenv("DFETCH_TEST_CRED", "set-after-new")
	v, err := c.Get(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "set-after-new", v)
}

// Concurrent Gets are race-safe: the engine scans a connector's tables in
// parallel, so first use can be concurrent (run with -race).
func TestCredentialConcurrent(t *testing.T) {
	c := mustCredential(t, "auth_header",
		map[string]any{"auth_header_command": []any{"echo", "Bearer from-command"}}, "", nil)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := c.Get(context.Background())
			assert.NoError(t, err)
			assert.Equal(t, "Bearer from-command", h)
		}()
	}
	wg.Wait()
}

// A failing command surfaces its stderr in the error.
func TestCredentialCommandStderr(t *testing.T) {
	c := mustCredential(t, "token",
		map[string]any{"token_command": []any{"sh", "-c", "echo boom >&2; exit 3"}}, "", nil)
	_, err := c.Get(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

func TestEnvFirst(t *testing.T) {
	t.Setenv("DFETCH_TEST_A", "")
	t.Setenv("DFETCH_TEST_B", "b-value")
	assert.Equal(t, "b-value", EnvFirst("DFETCH_TEST_A", "DFETCH_TEST_B")())
	assert.Empty(t, EnvFirst("DFETCH_TEST_A")())
	t.Setenv("DFETCH_TEST_A", "a-value")
	assert.Equal(t, "a-value", EnvFirst("DFETCH_TEST_A", "DFETCH_TEST_B")())
}
