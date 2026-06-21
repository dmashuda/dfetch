// Command examples drives internal/examples: it regenerates the README example
// blocks from examples.yaml (-mode gen), verifies they're in sync (-mode check),
// or runs every example query against the live services (-mode run). It is a dev
// tool, not part of the dfetch binary; invoke it via the `make examples*` targets.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dmashuda/dfetch/internal/examples"
)

func main() {
	mode := flag.String("mode", "gen", "gen | check | run")
	yamlPath := flag.String("yaml", "examples.yaml", "path to examples.yaml")
	readmePath := flag.String("readme", "README.md", "path to README.md")
	bin := flag.String("bin", "./bin/dfetch", "dfetch binary for -mode run")
	jaegerURL := flag.String("jaeger", "http://localhost:16686", "Jaeger base URL probed for -mode run")
	flag.Parse()

	f, err := examples.Load(*yamlPath)
	if err != nil {
		fatal(err)
	}

	switch *mode {
	case "gen":
		if err := generate(*readmePath, f); err != nil {
			fatal(err)
		}
		fmt.Printf("regenerated README example blocks from %s\n", *yamlPath)
	case "check":
		if err := check(*readmePath, f); err != nil {
			fatal(err)
		}
		fmt.Println("README examples are in sync with " + *yamlPath)
	case "run":
		os.Exit(runAll(f, *bin, *jaegerURL))
	default:
		fatal(fmt.Errorf("unknown -mode %q (want gen|check|run)", *mode))
	}
}

func generate(readmePath string, f examples.File) error {
	cur, err := os.ReadFile(readmePath)
	if err != nil {
		return err
	}
	out, err := examples.Apply(string(cur), f)
	if err != nil {
		return err
	}
	if out == string(cur) {
		return nil
	}
	return os.WriteFile(readmePath, []byte(out), 0o644)
}

func check(readmePath string, f examples.File) error {
	cur, err := os.ReadFile(readmePath)
	if err != nil {
		return err
	}
	out, err := examples.Apply(string(cur), f)
	if err != nil {
		return err
	}
	if out != string(cur) {
		return fmt.Errorf("%s is out of date with the examples — run `make examples`", readmePath)
	}
	return nil
}

func runAll(f examples.File, bin, jaegerURL string) int {
	jaegerUp := reachable(jaegerURL)
	token := ghToken()
	var pass, fail, skip int
	for _, g := range f.Groups {
		for _, e := range g.Examples {
			label := g.Name + ": " + e.Desc
			switch {
			case !e.Runnable():
				fmt.Printf("SKIP  %s (not runnable as-is)\n", label)
				skip++
			case g.Requires == "jaeger" && !jaegerUp:
				fmt.Printf("SKIP  %s (no Jaeger at %s)\n", label, jaegerURL)
				skip++
			default:
				if out, err := runQuery(bin, e.Query, token); err != nil {
					fmt.Printf("FAIL  %s\n%s\n", label, indent(out))
					fail++
				} else {
					fmt.Printf("PASS  %s\n", label)
					pass++
				}
			}
		}
	}
	fmt.Printf("\n%d passed, %d failed, %d skipped\n", pass, fail, skip)
	if fail > 0 {
		return 1
	}
	return 0
}

func runQuery(bin, query, token string) (string, error) {
	cmd := exec.Command(bin, "query", query)
	cmd.Env = os.Environ()
	if token != "" {
		cmd.Env = append(cmd.Env, "GITHUB_TOKEN="+token)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ghToken best-effort fetches a GitHub token from the gh CLI so authenticated
// (un-rate-limited) requests are used when available.
func ghToken() string {
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func reachable(baseURL string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(baseURL)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, ln := range lines {
		lines[i] = "      " + ln
	}
	return strings.Join(lines, "\n")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "examples:", err)
	os.Exit(1)
}
