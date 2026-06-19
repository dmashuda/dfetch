package source

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultRegistryBuildsCSV(t *testing.T) {
	r := DefaultRegistry()
	s, err := r.Build("csv", "users", map[string]any{"path": "./users.csv"})
	require.NoError(t, err)

	_, err = s.Schema(context.Background())
	assert.ErrorIs(t, err, ErrNotImplemented)
}

func TestRegistryBuildUnknownType(t *testing.T) {
	_, err := NewRegistry().Build("nope", "t", nil)
	assert.Error(t, err)
}

func TestRegistryRegisterDuplicatePanics(t *testing.T) {
	r := NewRegistry()
	r.Register("csv", NewCSVSource)
	assert.Panics(t, func() {
		r.Register("csv", NewCSVSource)
	})
}
