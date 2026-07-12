package connectors

import (
	"testing"

	"github.com/dmashuda/dfetch/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every default type — builtin and config-only — builds from the registry.
func TestDefaultRegistryKnowsAllTypes(t *testing.T) {
	reg := DefaultRegistry()
	for _, typeName := range []string{"datagov", "docker", "git", "github", "jaeger", "slack"} {
		c, err := reg.Build(typeName, nil)
		require.NoError(t, err, typeName)
		assert.NotNil(t, c, typeName)
	}
	// Config-only types are registered too; they may reject nil params, but they
	// must not be unknown.
	for _, typeName := range []string{"jira", "newrelic", "postgres"} {
		_, err := reg.Build(typeName, nil)
		if err != nil {
			assert.NotContains(t, err.Error(), "unknown connector type", typeName)
		}
	}
}

func TestNewBuiltinsInstantiatesEverySchema(t *testing.T) {
	conns, err := NewBuiltins()
	require.NoError(t, err)
	for _, schema := range []string{"datagov", "docker", "git", "github", "jaeger", "slack"} {
		assert.Contains(t, conns, schema)
	}
	assert.Len(t, conns, 6)
}

// DefaultOptions reproduces the stock CLI setup: an engine where the builtin
// schemas resolve (successor to the old engine.New builtin test).
func TestDefaultOptionsBuildEngine(t *testing.T) {
	opts, err := DefaultOptions()
	require.NoError(t, err)

	e, err := engine.New(opts...)
	require.NoError(t, err)

	names, err := e.ListTables(t.Context(), "github", "issues")
	require.NoError(t, err)
	assert.Contains(t, names, "issues")
}
