package mtix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_Ping_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-agent")
	err := client.Ping(context.Background())
	assert.NoError(t, err)
}

func TestClient_Ping_Failure(t *testing.T) {
	client := NewClient("http://127.0.0.1:1", "test-agent") // nothing listening
	err := client.Ping(context.Background())
	assert.Error(t, err)
}

func TestClient_GetNode_Success(t *testing.T) {
	node := Node{
		ID:     "MGIT-1.2.3",
		Title:  "Test task",
		Status: "open",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "test-agent", r.Header.Get("X-Agent-ID"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(node)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-agent")
	got, err := client.GetNode(context.Background(), "MGIT-1.2.3")
	require.NoError(t, err)
	assert.Equal(t, "MGIT-1.2.3", got.ID)
	assert.Equal(t, "Test task", got.Title)
}

func TestClient_GetNode_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-agent")
	_, err := client.GetNode(context.Background(), "MGIT-99.99")
	assert.Error(t, err)
}

func TestClient_MarkDone_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "mtix", r.Header.Get("X-Requested-With"))
		assert.Equal(t, http.MethodPost, r.Method)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-agent")
	err := client.MarkDone(context.Background(), "MGIT-1.2.3")
	assert.NoError(t, err)
}

func TestClient_MarkDone_Failure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"invalid transition"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-agent")
	err := client.MarkDone(context.Background(), "MGIT-1.2.3")
	assert.Error(t, err)
}

func TestClient_DefaultURL(t *testing.T) {
	client := NewClient("", "agent")
	assert.Equal(t, DefaultURL, client.baseURL)
}
