package engine

import (
	"strings"
	"testing"
)

func sampleResult() *Result {
	return &Result{
		Columns: []string{"id", "name"},
		Rows:    [][]any{{1, "alice"}, {2, "bob"}},
	}
}

func TestResultWriteTable(t *testing.T) {
	var sb strings.Builder
	if err := sampleResult().Write(&sb, "table"); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "id\tname") || !strings.Contains(out, "1\talice") {
		t.Fatalf("unexpected table output:\n%s", out)
	}
}

func TestResultWriteJSON(t *testing.T) {
	var sb strings.Builder
	if err := sampleResult().Write(&sb, "json"); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, `"name": "alice"`) {
		t.Fatalf("unexpected json output:\n%s", out)
	}
}

func TestResultWriteCSV(t *testing.T) {
	var sb strings.Builder
	if err := sampleResult().Write(&sb, "csv"); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "id,name") || !strings.Contains(out, "2,bob") {
		t.Fatalf("unexpected csv output:\n%s", out)
	}
}

func TestResultWriteUnknownFormat(t *testing.T) {
	if err := sampleResult().Write(&strings.Builder{}, "xml"); err == nil {
		t.Fatal("expected error for unknown format")
	}
}
