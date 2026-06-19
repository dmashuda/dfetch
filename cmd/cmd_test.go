package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	SetVersion("v9.9.9-test")

	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"version"})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("version command failed: %v", err)
	}

	if got := strings.TrimSpace(out.String()); got != "v9.9.9-test" {
		t.Fatalf("version output = %q, want %q", got, "v9.9.9-test")
	}
}
