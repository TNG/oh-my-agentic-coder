package cli

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/tngtech/oh-my-agentic-coder/internal/manifest"
)

// runManifest renders the skills manifest from activate-response JSON.
// It reads JSON from --input <file> or stdin, and writes the rendered
// markdown to stdout. On parse failure it prints a warning to stderr but
// returns ExitOK, mirroring manifest.Render() which returns "" on error.
func runManifest(args []string, env *Env) int {
	fs := flag.NewFlagSet("manifest", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	skillsDir := fs.String("skills-dir", "", "active harness skills dir (required)")
	input := fs.String("input", "", "activate-response JSON file (default: stdin)")
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac manifest --skills-dir <dir> [--input <file>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return ExitMisuse
	}
	if *skillsDir == "" {
		fmt.Fprintln(env.Stderr, "omac manifest: --skills-dir is required")
		return ExitMisuse
	}

	var data []byte
	var err error
	if *input != "" {
		data, err = os.ReadFile(*input)
		if err != nil {
			fmt.Fprintf(env.Stderr, "omac manifest: read %s: %v\n", *input, err)
			return ExitIOError
		}
	} else {
		data, err = io.ReadAll(env.Stdin)
		if err != nil {
			fmt.Fprintf(env.Stderr, "omac manifest: read stdin: %v\n", err)
			return ExitIOError
		}
	}

	out := manifest.Render(string(data), *skillsDir)
	if out == "" && len(data) > 0 {
		fmt.Fprintln(env.Stderr, "omac manifest: warning: failed to parse activate-response JSON")
	}
	fmt.Fprint(env.Stdout, out)
	return ExitOK
}
