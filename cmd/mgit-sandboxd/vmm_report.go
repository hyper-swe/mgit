package main

import (
	"encoding/json"
	"io"

	"github.com/hyper-swe/mgit/internal/model"
)

// writeVMMReport prints the --vmm report as one JSON object and returns the
// exit code. A report that names problems still exits 0: the question was
// answered, and the answer is the problems. Refs: MGIT-229
func writeVMMReport(out io.Writer, r model.VMMReport) int {
	if err := json.NewEncoder(out).Encode(r); err != nil {
		return 1
	}
	return 0
}
