// Command gen writes the generated regions of the check documentation.
//
// Run through `go generate ./...` or `just generate`. The output is checked
// in, and `just check` regenerates and fails on a diff, so a check whose
// default changed cannot ship with a page that still states the old one
// (spec 7.8).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dennisme/clickhouse-ruler/internal/docs"
)

func main() {
	out := flag.String("out", "docs/checks", "directory holding the check pages")
	flag.Parse()

	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(dir string) error {
	for _, page := range docs.Pages {
		path := filepath.Join(dir, page)

		content, err := os.ReadFile(path) //nolint:gosec // a path this repository owns
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}

		updated, err := docs.Apply(page, string(content))
		if err != nil {
			return err
		}
		// The path is -out plus a page name from this package's own list, and
		// this command is run by a developer against a checked-out tree.
		if err := os.WriteFile(path, []byte(updated), 0o600); err != nil { //nolint:gosec // a developer-supplied output directory is the input
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}

	index := filepath.Join(dir, "index.md")
	if err := os.WriteFile(index, []byte(docs.Index()), 0o600); err != nil { //nolint:gosec // as above
		return fmt.Errorf("writing %s: %w", index, err)
	}
	return nil
}
