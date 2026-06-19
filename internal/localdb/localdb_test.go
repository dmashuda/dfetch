package localdb

import (
	"context"
	"testing"
)

func TestOpenClose(t *testing.T) {
	db, err := Open(context.Background())
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
}
