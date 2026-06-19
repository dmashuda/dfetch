package localdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenClose(t *testing.T) {
	db, err := Open(context.Background())
	require.NoError(t, err)
	require.NoError(t, db.Close())
}
