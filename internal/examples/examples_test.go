package examples

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func boolPtr(b bool) *bool { return &b }

func TestRunnable(t *testing.T) {
	assert.True(t, Example{}.Runnable()) // nil → true
	assert.True(t, Example{Run: boolPtr(true)}.Runnable())
	assert.False(t, Example{Run: boolPtr(false)}.Runnable())
}

func TestRenderExampleAlignsAndEscapes(t *testing.T) {
	e := Example{
		Desc: "error spans, JSON column",
		Query: "SELECT operation_name,\n" +
			"       json_extract(attributes, '$.\"db.statement\"') AS sql\n" +
			"FROM jaeger.spans",
	}
	got := renderExample(e)
	want := "# error spans, JSON column\n" +
		"dfetch query \"SELECT operation_name,\n" +
		"              " + "       json_extract(attributes, '\\$.\\\"db.statement\\\"') AS sql\n" +
		"              FROM jaeger.spans\""
	assert.Equal(t, want, got)
}

func TestRenderBlockJoinsWithBlankLine(t *testing.T) {
	g := Group{Name: "x", Examples: []Example{
		{Desc: "a", Query: "SELECT 1"},
		{Desc: "b", Query: "SELECT 2"},
	}}
	want := "```sh\n" +
		"# a\ndfetch query \"SELECT 1\"\n\n" +
		"# b\ndfetch query \"SELECT 2\"\n" +
		"```"
	assert.Equal(t, want, RenderBlock(g))
}

func TestApplyReplacesBetweenMarkersAndIsIdempotent(t *testing.T) {
	readme := "intro\n\n<!-- BEGIN EXAMPLES x -->\nstale\n<!-- END EXAMPLES x -->\n\noutro\n"
	f := File{Groups: []Group{{Name: "x", Examples: []Example{{Desc: "a", Query: "SELECT 1"}}}}}

	out, err := Apply(readme, f)
	require.NoError(t, err)
	assert.Contains(t, out, "# a\ndfetch query \"SELECT 1\"")
	assert.NotContains(t, out, "stale")
	assert.True(t, len(out) > 0 && out[len(out)-len("outro\n"):] == "outro\n")

	// Applying again is a no-op (generation is stable).
	again, err := Apply(out, f)
	require.NoError(t, err)
	assert.Equal(t, out, again)
}

func TestApplyMissingMarkerErrors(t *testing.T) {
	_, err := Apply("no markers here", File{Groups: []Group{{Name: "x"}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BEGIN EXAMPLES x")
}

func TestLoadRepoExamples(t *testing.T) {
	// The committed examples.yaml parses and has the expected groups.
	f, err := Load(filepath.Join("..", "..", "examples.yaml"))
	require.NoError(t, err)
	names := make([]string, 0, len(f.Groups))
	for _, g := range f.Groups {
		names = append(names, g.Name)
		assert.NotEmpty(t, g.Examples, "group %s has examples", g.Name)
	}
	assert.Equal(t, []string{"github", "jaeger"}, names)
}
