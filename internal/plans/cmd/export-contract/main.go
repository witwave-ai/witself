// Command export-contract writes the Worker plan validator contract from Go.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/witwave-ai/witself/internal/plans"
)

func main() {
	output := flag.String("output", "infra/cloudflare/control-plane/src/plan-contract.json", "generated contract path")
	flag.Parse()
	data, err := plans.WorkerValidationContractJSON()
	if err == nil {
		err = os.WriteFile(*output, data, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
