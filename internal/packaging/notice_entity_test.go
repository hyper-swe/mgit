package packaging

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// NOTICE names the legal entity that holds the copyright, not only the
// product name. The entity name is the founder's interim wording and the
// country is not shown until he confirms it. Refs: MGIT-306
func TestNotice_NamesTheCopyrightHolderAsALegalEntity(t *testing.T) {
	const entity = "Astutic Artificial Intelligence Private Limited"
	lines := strings.Split(readRepoFile(t, "NOTICE"), "\n")
	assert.Equal(t, "Copyright 2025-2026 "+entity, lines[0], "the first line must name the copyright holder")
	assert.Contains(t, strings.Join(lines[:4], "\n"), "software developed by "+entity)
}

// The appendix line of LICENSE names the same holder as NOTICE.
// Refs: MGIT-306
func TestLicense_AppendixNamesTheCopyrightHolderAsALegalEntity(t *testing.T) {
	const entity = "Astutic Artificial Intelligence Private Limited"
	assert.Contains(t, "\n"+readRepoFile(t, "LICENSE"), "\n   Copyright 2025-2026 "+entity+"\n")
}
