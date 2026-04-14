package mtix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegration_GetTaskInfo_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	integration := setupIntegration(t, srv)
	_, err := integration.GetTaskInfo(context.Background(), "MGIT-99.9")
	assert.Error(t, err)
}

func TestIntegration_WatchEvents_CancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	integration := setupIntegration(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := integration.WatchEvents(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestEvent_JSONSerialization(t *testing.T) {
	evt := Event{
		Type:      "node.status_changed",
		NodeID:    "MGIT-1.2.3",
		Timestamp: "2026-04-08T12:00:00Z",
		Author:    "agent-01",
		Data:      json.RawMessage(`{"old":"open","new":"done"}`),
	}

	data, err := json.Marshal(evt)
	require.NoError(t, err)

	var restored Event
	require.NoError(t, json.Unmarshal(data, &restored))
	assert.Equal(t, "node.status_changed", restored.Type)
	assert.Equal(t, "MGIT-1.2.3", restored.NodeID)
}

func TestClient_GetNode_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{invalid json`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-agent")
	_, err := client.GetNode(context.Background(), "MGIT-1.1")
	assert.Error(t, err)
}

func TestClient_MarkDone_RequestHeaders(t *testing.T) {
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "alpha-agent")
	err := client.MarkDone(context.Background(), "MGIT-1.1")
	require.NoError(t, err)

	assert.Equal(t, "alpha-agent", gotHeaders.Get("X-Agent-ID"))
	assert.Equal(t, "mtix", gotHeaders.Get("X-Requested-With"))
}
