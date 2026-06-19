package source

import (
	"context"
	"errors"
	"testing"
)

func TestDefaultRegistryBuildsCSV(t *testing.T) {
	r := DefaultRegistry()
	s, err := r.Build("csv", "users", map[string]any{"path": "./users.csv"})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if _, err := s.Schema(context.Background()); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("expected ErrNotImplemented from stub, got %v", err)
	}
}

func TestRegistryBuildUnknownType(t *testing.T) {
	if _, err := NewRegistry().Build("nope", "t", nil); err == nil {
		t.Fatal("expected error for unknown source type")
	}
}

func TestRegistryRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	r := NewRegistry()
	r.Register("csv", NewCSVSource)
	r.Register("csv", NewCSVSource)
}
