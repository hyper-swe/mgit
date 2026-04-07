// Package mtix implements the mtix client integration for mgit.
// Connects to the mtix REST API at localhost:6849 for bidirectional
// task-commit synchronization.
// Refs: FR-14, MGIT-5.3.1
package mtix

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultURL is the default mtix API endpoint.
const DefaultURL = "http://127.0.0.1:6849"

// Node represents an mtix task node (subset of fields relevant to mgit).
// Refs: FR-14
type Node struct {
	ID        string `json:"id"`
	ParentID  string `json:"parent_id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	Assignee  string `json:"assignee"`
	NodeType  string `json:"node_type"`
	Priority  int    `json:"priority"`
	Progress  int    `json:"progress"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Client wraps the mtix HTTP API.
// Refs: FR-14, MGIT-5.3.1
type Client struct {
	baseURL    string
	httpClient *http.Client
	agentID    string
}

// NewClient creates an mtix client connecting to the given URL.
func NewClient(baseURL, agentID string) *Client {
	if baseURL == "" {
		baseURL = DefaultURL
	}
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		agentID: agentID,
	}
}

// GetNode retrieves a task node by ID.
// Refs: FR-14
func (c *Client) GetNode(ctx context.Context, id string) (*Node, error) {
	url := fmt.Sprintf("%s/api/v1/nodes/%s", c.baseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("X-Agent-ID", c.agentID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get node %s: %w", id, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get node %s: status %d: %s", id, resp.StatusCode, string(body))
	}

	var node Node
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
		return nil, fmt.Errorf("decode node %s: %w", id, err)
	}
	return &node, nil
}

// MarkDone marks a task node as done via the mtix API.
// Refs: FR-14
func (c *Client) MarkDone(ctx context.Context, id string) error {
	url := fmt.Sprintf("%s/api/v1/nodes/%s/done", c.baseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("X-Agent-ID", c.agentID)
	req.Header.Set("X-Requested-With", "mtix")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("mark done %s: %w", id, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mark done %s: status %d: %s", id, resp.StatusCode, string(body))
	}
	return nil
}

// Ping checks if the mtix server is reachable.
func (c *Client) Ping(ctx context.Context) error {
	url := fmt.Sprintf("%s/health", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ping mtix: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ping mtix: status %d", resp.StatusCode)
	}
	return nil
}
