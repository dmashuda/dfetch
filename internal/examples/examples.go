// Package examples renders the runnable query examples in examples.yaml into a
// Markdown doc's marked example blocks (connectors.md), and is the basis for
// verifying (examples-check) and running (examples-test) those queries.
// examples.yaml is the single source of truth; the blocks between
// <!-- BEGIN/END EXAMPLES <name> --> markers are generated from it. See
// tools/examples for the CLI that drives this package.
package examples

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// contIndent aligns continuation lines under the opening quote of `dfetch query "`
// (13 chars + the quote = 14 columns), matching the hand-written README style.
const contIndent = "              "

// Example is one documented query.
type Example struct {
	Desc  string `yaml:"desc"`
	Query string `yaml:"query"`
	// Run is whether examples-test should execute this query; nil means true.
	// Set false for queries that can't run as-is (e.g. a placeholder value).
	Run *bool `yaml:"run"`
}

// Runnable reports whether examples-test should execute this example.
func (e Example) Runnable() bool { return e.Run == nil || *e.Run }

// Group is a set of examples that maps to one README marker region.
type Group struct {
	Name string `yaml:"name"`
	// Requires names a prerequisite for running the group's queries in
	// examples-test ("jaeger" → a reachable local Jaeger); empty means none.
	Requires string    `yaml:"requires"`
	Examples []Example `yaml:"examples"`
}

// File is the parsed examples.yaml.
type File struct {
	Groups []Group `yaml:"groups"`
}

// Load reads and parses examples.yaml.
func Load(path string) (File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return File{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return f, nil
}

// shellEscape escapes a string for inclusion in a double-quoted shell argument,
// so the rendered `dfetch query "…"` is copy-pasteable.
func shellEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `$`, `\$`).Replace(s)
}

// renderExample renders one example as a commented `dfetch query "…"` invocation,
// aligning continuation lines under the opening quote.
func renderExample(e Example) string {
	lines := strings.Split(strings.TrimRight(e.Query, "\n"), "\n")
	var b strings.Builder
	b.WriteString("# " + e.Desc + "\n")
	for i, ln := range lines {
		esc := shellEscape(ln)
		if i == 0 {
			b.WriteString(`dfetch query "` + esc)
			continue
		}
		b.WriteString("\n" + contIndent + esc)
	}
	b.WriteByte('"')
	return b.String()
}

// RenderBlock renders a group's fenced ```sh example block (without the markers).
func RenderBlock(g Group) string {
	parts := make([]string, len(g.Examples))
	for i, e := range g.Examples {
		parts[i] = renderExample(e)
	}
	return "```sh\n" + strings.Join(parts, "\n\n") + "\n```"
}

// Apply replaces each group's marker region in the doc with its rendered block
// and returns the updated doc. It errors if a group's markers are missing, so a
// renamed/typo'd marker fails loudly instead of silently dropping examples.
func Apply(readme string, f File) (string, error) {
	for _, g := range f.Groups {
		var err error
		readme, err = replaceBlock(readme, g.Name, RenderBlock(g))
		if err != nil {
			return "", err
		}
	}
	return readme, nil
}

func replaceBlock(readme, name, block string) (string, error) {
	begin := "<!-- BEGIN EXAMPLES " + name + " -->"
	end := "<!-- END EXAMPLES " + name + " -->"
	bi := strings.Index(readme, begin)
	if bi < 0 {
		return "", fmt.Errorf("missing marker %q in the examples doc", begin)
	}
	ei := strings.Index(readme, end)
	if ei < 0 {
		return "", fmt.Errorf("missing marker %q in the examples doc", end)
	}
	if ei < bi {
		return "", fmt.Errorf("marker %q appears before %q", end, begin)
	}
	return readme[:bi+len(begin)] + "\n" + block + "\n" + readme[ei:], nil
}
