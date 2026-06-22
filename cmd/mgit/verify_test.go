package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReportVerifyResult_CleanRepo_ExitZero verifies that a clean verify
// (no issues) prints the success line and returns no error (exit 0).
// Refs: MGIT-21
func TestReportVerifyResult_CleanRepo_ExitZero(t *testing.T) {
	var out, errOut bytes.Buffer
	err := reportVerifyResult(&out, &errOut, nil, false)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "All checks passed")
}

// TestReportVerifyResult_IssuesFound_ExitNonZero verifies that when verify
// finds any issue it prints the warnings, the count, and returns an
// exitError with a non-zero code so CI/scripts can detect the failure.
// Refs: MGIT-21
func TestReportVerifyResult_IssuesFound_ExitNonZero(t *testing.T) {
	var out, errOut bytes.Buffer
	issues := []string{"git commit deadbeef has no index entry"}

	err := reportVerifyResult(&out, &errOut, issues, false)

	require.Error(t, err, "verify with issues must return a non-zero exit")
	var ee *exitError
	require.True(t, errors.As(err, &ee), "error must be an exitError")
	assert.NotZero(t, ee.code, "exit code must be non-zero")
	assert.Contains(t, errOut.String(), "WARNING: git commit deadbeef has no index entry")
	assert.Contains(t, out.String(), "1 issues found")
}

// TestReportVerifyResult_JSON_IssuesFound_ExitNonZero verifies the JSON
// output path still emits machine-readable output AND exits non-zero when
// issues are present. Refs: MGIT-21
func TestReportVerifyResult_JSON_IssuesFound_ExitNonZero(t *testing.T) {
	var out, errOut bytes.Buffer
	issues := []string{"git commit deadbeef has no index entry"}

	err := reportVerifyResult(&out, &errOut, issues, true)

	require.Error(t, err)
	var ee *exitError
	require.True(t, errors.As(err, &ee))
	assert.Contains(t, out.String(), `"ok":false`)
}

// TestReportVerifyResult_JSON_Clean_ExitZero verifies clean JSON output
// exits 0. Refs: MGIT-21
func TestReportVerifyResult_JSON_Clean_ExitZero(t *testing.T) {
	var out, errOut bytes.Buffer
	err := reportVerifyResult(&out, &errOut, nil, true)
	require.NoError(t, err)
	assert.Contains(t, out.String(), `"ok":true`)
}
