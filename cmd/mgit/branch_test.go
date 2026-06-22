package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIsBranchListArg verifies the bare-list aliases are recognized so
// `mgit branch list` lists branches instead of trying to switch to a
// branch literally named "list". Refs: MGIT-23
func TestIsBranchListArg(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "no_args_is_list", args: nil, want: true},
		{name: "list_subcommand", args: []string{"list"}, want: true},
		{name: "ls_alias", args: []string{"ls"}, want: true},
		{name: "real_branch_name", args: []string{"task/MGIT-1.2"}, want: false},
		{name: "list_with_extra_arg_is_switch", args: []string{"list", "x"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isBranchListArg(tt.args))
		})
	}
}
