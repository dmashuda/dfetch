package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// CredentialFunc lazily supplies a credential value — a token, a header value,
// or a DSN. Passed through params (e.g. params["token_func"]) by programs that
// build their config in Go; YAML config cannot express it.
type CredentialFunc func(ctx context.Context) (string, error)

// Credential resolves one connector secret lazily, once, race-safely, from up
// to four sources with fixed precedence:
//
//  1. a static param value (params["<staticKey>"], e.g. postgres's "dsn"),
//  2. the environment (via the env closure — read at first Get, not at
//     construction),
//  3. a caller-supplied Go function (params["<name>_func"]),
//  4. an executed command (params["<name>_command"] — an argv list run
//     directly without a shell, 5s timeout, stdout's trailing newline
//     trimmed).
//
// Connectors are built eagerly for every query (engine.New), so resolution is
// deferred to first use to avoid shelling out (or calling a func) on queries
// that never touch the connector. All paths run inside a sync.Once — the
// engine scans a connector's tables concurrently, and Once.Do is what makes
// the closure's write visible to every other goroutine's read (a bare
// fast-path read would race the writer). The first caller's ctx governs the
// func call / command timeout. Both the value and any error are cached.
//
// An empty value with a nil error means nothing is configured (or the env
// var is unset); each connector decides whether that is an error or an
// unauthenticated request.
type Credential struct {
	name   string        // param base name, e.g. "token", "auth_header" — labels errors
	static string        // params[staticKey] when the connector declares a static param
	env    func() string // returns "" when unset; may shape the value (e.g. "Bearer "+token)
	fn     CredentialFunc
	cmd    []string

	once sync.Once
	val  string
	err  error
}

// NewCredential builds a Credential for the param base name: it reads
// params[name+"_func"] (which must be a source.CredentialFunc) and
// params[name+"_command"] (an argv list of strings). staticKey, when
// non-empty, names a plain string param holding the value directly (e.g.
// "dsn"). env, when non-nil, reads the environment at resolve time; use
// EnvFirst for the common first-non-empty-var case. connector prefixes
// parse errors (e.g. "github: token_command must be a list of strings").
func NewCredential(connector, name string, params map[string]any, staticKey string, env func() string) (*Credential, error) {
	c := &Credential{name: name, env: env}
	if staticKey != "" {
		if v, ok := params[staticKey].(string); ok {
			c.static = v
		}
	}
	if raw, ok := params[name+"_func"]; ok {
		fn, ok := raw.(CredentialFunc)
		if !ok {
			// Also accept the underlying func type, so callers don't have to
			// convert to the named type explicitly.
			plain, okPlain := raw.(func(context.Context) (string, error))
			if !okPlain {
				return nil, fmt.Errorf("%s: %s_func must be a func(context.Context) (string, error)", connector, name)
			}
			fn = plain
		}
		c.fn = fn
	}
	if raw, ok := params[name+"_command"]; ok {
		cmd, err := stringListParam(connector, name+"_command", raw)
		if err != nil {
			return nil, err
		}
		c.cmd = cmd
	}
	return c, nil
}

// Get resolves the credential on first call and returns the cached value (and
// error) on every later call. See the Credential doc for precedence and
// concurrency semantics.
func (c *Credential) Get(ctx context.Context) (string, error) {
	c.once.Do(func() {
		if c.static != "" {
			c.val = c.static
			return
		}
		if c.env != nil {
			if v := c.env(); v != "" {
				c.val = v
				return
			}
		}
		if c.fn != nil {
			c.val, c.err = c.fn(ctx)
			return
		}
		if len(c.cmd) > 0 {
			c.val, c.err = runCommand(ctx, c.name+"_command", c.cmd)
		}
	})
	return c.val, c.err
}

// EnvFirst returns a closure over the environment that yields the first
// non-empty variable among vars, or "" when none is set. It is the env source
// for the common bare-value case; connectors that shape the value (a "Bearer "
// prefix, a Basic pair) write their own closure.
func EnvFirst(vars ...string) func() string {
	return func() string {
		for _, k := range vars {
			if v := os.Getenv(k); v != "" {
				return v
			}
		}
		return ""
	}
}

// runCommand runs cmd and returns its stdout (trailing newline trimmed). name
// labels the param in errors (e.g. "auth_header_command").
func runCommand(ctx context.Context, name string, cmd []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// #nosec G204 -- the command is explicit user configuration and is run
	// directly without a shell.
	out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			stderr := strings.TrimRight(string(ee.Stderr), "\n")
			if stderr != "" {
				return "", fmt.Errorf("%s %q: %w: %s", name, cmd, err, stderr)
			}
		}
		return "", fmt.Errorf("%s %q: %w", name, cmd, err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// stringListParam reads a param as a non-empty list of non-empty strings,
// tolerating the []any shape YAML produces. connector and name label errors.
func stringListParam(connector, name string, raw any) ([]string, error) {
	switch v := raw.(type) {
	case []string:
		return cleanStringList(connector, name, v)
	case []any:
		items := make([]string, len(v))
		for i, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s: %s[%d] must be a string", connector, name, i)
			}
			items[i] = s
		}
		return cleanStringList(connector, name, items)
	default:
		return nil, fmt.Errorf("%s: %s must be a list of strings", connector, name)
	}
}

func cleanStringList(connector, name string, items []string) ([]string, error) {
	out := make([]string, 0, len(items))
	for i, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("%s: %s[%d] must not be empty", connector, name, i)
		}
		out = append(out, item)
	}
	return out, nil
}
