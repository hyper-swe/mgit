package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Only historical descriptions written by the dependency-update app are
// exempt. A corrected description must not erase a human's earlier hit.
func TestFetch_DescriptionHistoryEditor(t *testing.T) {
	for _, tt := range []struct {
		name, editor, body, field string
		want                      int
	}{
		{"dependency app old description", `{"__typename":"Bot","login":"dependabot"}`, "clean", "description", 0},
		{"human old description", `{"__typename":"User","login":"maintainer"}`, "clean", "description", 1},
		{"unknown editor", `null`, "clean", "description", 1},
		{"other bot", `{"__typename":"Bot","login":"other[bot]"}`, "clean", "description", 1},
		{"user with similar login", `{"__typename":"User","login":"dependabot"}`, "clean", "description", 1},
		{"current dependency description", `{"__typename":"Bot","login":"dependabot"}`, testTerm, "description", 1},
		{"dependency app comment history", `{"__typename":"Bot","login":"dependabot"}`, "clean", "comment", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			history := testTerm
			if tt.body == testTerm {
				history = "clean"
			}
			node := fmt.Sprintf(`{"createdAt":"2026-10-13T11:00:00Z","body":%q,"userContentEdits":{"nodes":[{"editedAt":"2026-10-13T11:00:00Z","diff":%q,"editor":%s},{"editedAt":"2026-10-13T12:00:00Z","diff":"clean","editor":{"__typename":"User","login":"maintainer"}}]}}`, tt.body, history, tt.editor)
			call := func(q string, _ map[string]any) ([]byte, error) {
				assert.Contains(t, q, "editor{__typename login}", "the API must supply revision provenance")
				pr := strings.TrimSuffix(node, "}") + `,"title":"clean","author":{"__typename":"Bot","login":"dependabot"}}`
				if tt.field == "comment" {
					if q == queryPR {
						pr = `{"createdAt":"2026-10-13T11:00:00Z","title":"clean","body":"clean","comments":{"totalCount":1}}`
					} else {
						pr = `{"comments":{"nodes":[` + strings.TrimSuffix(node, "}") + `,"databaseId":7}]}}`
					}
				}
				return []byte(`{"data":{"repository":{"pullRequest":` + pr + `}}}`), nil
			}
			got, err := fetchPR(call, "owner", "repo", 7)
			require.NoError(t, err)
			var out bytes.Buffer
			code := report(&out, 7, got, judge(got.versions, testLists(), cutoff), cutoff)
			assert.Equal(t, tt.want, code, out.String())
		})
	}
}

// A current description is dated by its edit, not an old PR's creation.
// Unknown time remains gating, and a truncated history is never clean.
func TestFetch_CurrentDescriptionTimeAndOverflow(t *testing.T) {
	for _, tt := range []struct {
		name, stamp string
		more        bool
		want        int
	}{
		{"edited after cutoff", "2026-10-13T12:00:00Z", false, 1},
		{"edit time missing", "", false, 1},
		{"edited before cutoff", "2026-09-20T12:00:00Z", false, 0},
		{"unread bot history", "2026-10-13T12:00:00Z", true, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			call := func(string, map[string]any) ([]byte, error) {
				return []byte(fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"createdAt":"2026-09-20T11:00:00Z","lastEditedAt":%q,"title":"clean","body":%q,"userContentEdits":{"pageInfo":{"hasNextPage":%t},"nodes":[{"editedAt":"2026-09-20T11:00:00Z","diff":"clean","editor":{"__typename":"Bot","login":"dependabot"}}]}}}}}`, tt.stamp, testTerm, tt.more)), nil
			}
			var out bytes.Buffer
			code, _ := checkOne(call, options{owner: "owner", name: "repo", pr: 7, l: testLists()}, &out)
			assert.Equal(t, tt.want, code, out.String())
		})
	}
}
