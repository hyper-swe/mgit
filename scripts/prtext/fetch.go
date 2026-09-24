package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// The three queries read everything a pull request's text can carry: the
// title and its renames, the description and its edit history, and every
// issue comment, review and review comment with theirs. An edit-history
// entry's diff is the full text of that revision (measured on this
// repository: the oldest entry is the text as first written).
//
// THE API BUDGET IS SHARED. Every query reads the rate limit it leaves, the
// pull-request query counts comments and reviews so a query that would
// return nothing is never sent, and nested pages are small: a query's cost
// grows with the product of its nested page sizes, and a sweep of every
// pull request once spent a whole hour's budget. A page that overflows is
// NOT CHECKED, never skipped.
const (
	editsFields = `pageInfo{hasNextPage} nodes{editedAt deletedAt diff}`
	rateFields  = `rateLimit{remaining resetAt} `
	prHead      = rateFields + `repository(owner:$owner,name:$name){pullRequest(number:$number){`
	// GraphQL refuses a declared variable that goes unused, so only the
	// paged queries declare $after.
	queryHead = `query($owner:String!,$name:String!,$number:Int!){` + prHead
	pagedHead = `query($owner:String!,$name:String!,$number:Int!,$after:String){` + prHead
)

var queryPR = queryHead +
	`createdAt title body lastEditedAt userContentEdits(first:50){` + editsFields + `} comments{totalCount} reviews{totalCount} ` +
	`timelineItems(first:50,itemTypes:[RENAMED_TITLE_EVENT]){pageInfo{hasNextPage} nodes{... on RenamedTitleEvent{createdAt previousTitle currentTitle}}}}}}`

var queryComments = pagedHead +
	`comments(first:50,after:$after){pageInfo{hasNextPage endCursor} nodes{databaseId createdAt lastEditedAt body userContentEdits(first:20){` + editsFields + `}}}}}}`

var queryReviews = pagedHead +
	`reviews(first:20,after:$after){pageInfo{hasNextPage endCursor} nodes{databaseId createdAt lastEditedAt body userContentEdits(first:10){` + editsFields + `} ` +
	`comments(first:20){pageInfo{hasNextPage} nodes{databaseId createdAt lastEditedAt body userContentEdits(first:10){` + editsFields + `}}}}}}}}`

// api runs one GraphQL query with its variables and returns the raw JSON.
type api func(query string, vars map[string]any) ([]byte, error)

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type editConn struct {
	PageInfo pageInfo `json:"pageInfo"`
	Nodes    []struct {
		EditedAt  string  `json:"editedAt"`
		DeletedAt *string `json:"deletedAt"`
		Diff      *string `json:"diff"`
	} `json:"nodes"`
}

// textNode is any piece of text with an edit history.
type textNode struct {
	DatabaseID       int64    `json:"databaseId"`
	CreatedAt        string   `json:"createdAt"`
	Body             string   `json:"body"`
	UserContentEdits editConn `json:"userContentEdits"`
}

type prNode struct {
	textNode
	Title         string `json:"title"`
	TimelineItems struct {
		PageInfo pageInfo `json:"pageInfo"`
		Nodes    []struct {
			CreatedAt     string `json:"createdAt"`
			PreviousTitle string `json:"previousTitle"`
			CurrentTitle  string `json:"currentTitle"`
		} `json:"nodes"`
	} `json:"timelineItems"`
	Comments struct {
		TotalCount int        `json:"totalCount"`
		PageInfo   pageInfo   `json:"pageInfo"`
		Nodes      []textNode `json:"nodes"`
	} `json:"comments"`
	Reviews struct {
		TotalCount int      `json:"totalCount"`
		PageInfo   pageInfo `json:"pageInfo"`
		Nodes      []struct {
			textNode
			Comments struct {
				PageInfo pageInfo   `json:"pageInfo"`
				Nodes    []textNode `json:"nodes"`
			} `json:"comments"`
		} `json:"nodes"`
	} `json:"reviews"`
}

// collected is everything read from one pull request, and the API budget
// the last query left (nil when the answer did not say).
type collected struct {
	versions []version
	deleted  int
	counts   map[string]int
	budget   *rateLimit
}

// rateLimit is what the API says is left of the hour's budget.
type rateLimit struct {
	Remaining int    `json:"remaining"`
	ResetAt   string `json:"resetAt"`
}

var errMorePages = errors.New("more than one page of edit revisions or review comments; the check reads one, so this is not checked")

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// addText adds a piece of text and every revision in its history. Text
// with no history is one version, as written; empty text is nothing.
func (c *collected) addText(kind, field string, n *textNode) error {
	if n.UserContentEdits.PageInfo.HasNextPage {
		return fmt.Errorf("%s: %w", field, errMorePages)
	}
	c.counts[kind]++
	if len(n.UserContentEdits.Nodes) == 0 {
		if n.Body != "" {
			c.versions = append(c.versions, version{Field: field, Rev: "as written", At: parseTime(n.CreatedAt), Text: n.Body})
		}
		return nil
	}
	for _, e := range n.UserContentEdits.Nodes {
		if e.DeletedAt != nil || e.Diff == nil {
			c.deleted++
			continue
		}
		c.counts["revisions"]++
		c.versions = append(c.versions, version{Field: field, Rev: "revision", At: parseTime(e.EditedAt), Text: *e.Diff})
	}
	return nil
}

// addTitle adds the title as opened and as each rename left it.
func (c *collected) addTitle(pr *prNode) error {
	renames := pr.TimelineItems.Nodes
	if pr.TimelineItems.PageInfo.HasNextPage {
		return fmt.Errorf("title: %w", errMorePages)
	}
	sort.SliceStable(renames, func(i, j int) bool { return renames[i].CreatedAt < renames[j].CreatedAt })
	first := pr.Title
	if len(renames) > 0 {
		first = renames[0].PreviousTitle
	}
	c.versions = append(c.versions, version{Field: "title", Rev: "as opened", At: parseTime(pr.CreatedAt), Text: first})
	for _, r := range renames {
		c.versions = append(c.versions, version{Field: "title", Rev: "renamed", At: parseTime(r.CreatedAt), Text: r.CurrentTitle})
	}
	c.counts["title"] = 1 + len(renames)
	return nil
}

// query runs one query and decodes its pull request, refusing a GraphQL
// error or a missing pull request. The budget the answer reports, if any,
// is left in *budget.
func query(call api, q string, vars map[string]any, budget **rateLimit) (*prNode, error) {
	raw, err := call(q, vars)
	if err != nil {
		return nil, fmt.Errorf("the API could not be read: %w", err)
	}
	var resp struct {
		Data struct {
			RateLimit  *rateLimit `json:"rateLimit"`
			Repository struct {
				PullRequest *prNode `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("the API's answer is not JSON: %w", err)
	}
	if resp.Data.RateLimit != nil {
		*budget = resp.Data.RateLimit
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("the API answered with an error: %s", resp.Errors[0].Message)
	}
	if resp.Data.Repository.PullRequest == nil {
		return nil, errors.New("no pull request by that number")
	}
	return resp.Data.Repository.PullRequest, nil
}
