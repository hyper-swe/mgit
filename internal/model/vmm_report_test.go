package model

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVMMReport_CanBoot_OnlyWithoutProblems(t *testing.T) {
	tests := []struct {
		name   string
		report VMMReport
		want   bool
	}{
		{name: "no_problems", report: VMMReport{VMM: BackendLibkrun}, want: true},
		{name: "one_problem", report: VMMReport{VMM: BackendLibkrun, Problems: []string{"libkrunfw not loaded"}}, want: false},
		{name: "empty_vmm_is_never_bootable", report: VMMReport{}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.report.CanBoot())
		})
	}
}

// The report crosses a process boundary (daemon stdout → doctor), so its
// wire names are part of the contract.
func TestVMMReport_JSON_UsesSnakeCaseWireNames(t *testing.T) {
	r := VMMReport{
		VMM:       BackendLibkrun,
		Libraries: []VMMLibrary{{Name: "libkrunfw", Path: "/opt/mgit/lib/libkrunfw.so.5"}},
		Problems:  []string{"x"},
	}
	b, err := json.Marshal(r)
	require.NoError(t, err)
	assert.JSONEq(t, `{"vmm":"libkrun","libraries":[{"name":"libkrunfw","path":"/opt/mgit/lib/libkrunfw.so.5"}],"problems":["x"]}`, string(b))

	var back VMMReport
	require.NoError(t, json.Unmarshal(b, &back))
	assert.Equal(t, r, back)
}
