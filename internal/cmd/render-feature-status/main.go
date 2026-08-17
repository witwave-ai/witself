// Command render-feature-status regenerates docs/feature-status.md from the
// canonical feature status catalog.
package main

import (
	"fmt"
	"os"

	"github.com/witwave-ai/witself/featurestatus"
)

func main() {
	catalog, err := featurestatus.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := catalog.ValidateReferences("."); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile("docs/feature-status.md", featurestatus.RenderMarkdown(*catalog), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
