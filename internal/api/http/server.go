// Package http implements the REST API server for mgit.
// Endpoints call services — never stores directly.
// Refs: FR-9, NFR-5, MGIT-5.1.1
package http

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/oklog/ulid/v2"

	"github.com/hyper-swe/mgit-dev/internal/service"
	gitstore "github.com/hyper-swe/mgit-dev/internal/store/git"
	"github.com/hyper-swe/mgit-dev/internal/store/index"
)

// Server holds the Echo instance and all services.
// Refs: FR-9, MGIT-5.1.1
type Server struct {
	echo     *echo.Echo
	commit   *service.CommitService
	squash   *service.SquashService
	rollback *service.RollbackService
	branch   *service.BranchService
	verify   *service.VerifyService
	clock    func() time.Time
}

// NewServer creates a configured REST API server.
// Refs: FR-9, NFR-5
func NewServer(
	repo *gitstore.Repository,
	idx *index.Store,
	clock func() time.Time,
) *Server {
	cs := gitstore.NewCommitStore(repo)
	bs := gitstore.NewBranchStore(repo)

	s := &Server{
		echo:     echo.New(),
		commit:   service.NewCommitService(repo, cs, idx),
		squash:   service.NewSquashService(repo, cs, idx),
		rollback: service.NewRollbackService(repo, cs, idx),
		branch:   service.NewBranchService(repo, bs, idx),
		verify:   service.NewVerifyService(cs, idx),
		clock:    clock,
	}

	s.echo.HideBanner = true
	s.echo.HidePort = true

	s.setupMiddleware()
	s.setupRoutes()

	return s
}

// Start begins listening on the given address (e.g., "127.0.0.1:6860").
// Refs: FR-9, NFR-5 (localhost binding)
func (s *Server) Start(addr string) error {
	return s.echo.Start(addr)
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.echo.Shutdown(ctx)
}

// Echo returns the underlying Echo instance (for testing).
func (s *Server) Echo() *echo.Echo {
	return s.echo
}

// setupMiddleware configures the middleware chain.
// Refs: FR-9.1, NFR-5
func (s *Server) setupMiddleware() {
	// Request ID (X-Request-ID header with ULID)
	s.echo.Use(middleware.RequestIDWithConfig(middleware.RequestIDConfig{
		Generator: func() string {
			return ulid.Make().String()
		},
	}))

	// Logging
	s.echo.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogURI:    true,
		LogStatus: true,
		LogMethod: true,
		LogValuesFunc: func(_ echo.Context, _ middleware.RequestLoggerValues) error {
			return nil
		},
	}))

	// Recovery from panics
	s.echo.Use(middleware.Recover())
}

// setupRoutes registers all API endpoints.
// Refs: FR-9.2
func (s *Server) setupRoutes() {
	// Health check
	s.echo.GET("/health", s.healthHandler)

	// API v1 group
	v1 := s.echo.Group("/api/v1")

	// Commit endpoints (FR-9.2)
	v1.POST("/commits", s.createCommitHandler)
	v1.GET("/commits/:id", s.getCommitHandler)
	v1.GET("/commits", s.listCommitsHandler)
	v1.GET("/tasks/:id/commits", s.getTaskCommitsHandler)

	// Branch endpoints (FR-9.3)
	v1.GET("/branches", s.listBranchesHandler)
	v1.POST("/branches", s.createBranchHandler)

	// Squash and rollback (FR-9.4)
	v1.POST("/squash", s.squashHandler)
	v1.POST("/rollback", s.rollbackHandler)

	// Verify (FR-9.5)
	v1.GET("/verify", s.verifyHandler)
}

// healthHandler returns server health status.
func (s *Server) healthHandler(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]any{
		"status":    "ok",
		"timestamp": s.clock().UTC().Format(time.RFC3339),
	})
}

// createCommitHandler handles POST /api/v1/commits.
// Refs: FR-9.2
func (s *Server) createCommitHandler(c echo.Context) error {
	var req service.CreateCommitRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	commit, err := s.commit.CreateCommit(c.Request().Context(), req)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusCreated, commit)
}

// getCommitHandler handles GET /api/v1/commits/:id.
func (s *Server) getCommitHandler(c echo.Context) error {
	hash := c.Param("id")
	commit, err := s.commit.GetCommit(c.Request().Context(), hash)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, commit)
}

// listCommitsHandler handles GET /api/v1/commits.
func (s *Server) listCommitsHandler(c echo.Context) error {
	commits, err := s.commit.ListCommits(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, commits)
}

// getTaskCommitsHandler handles GET /api/v1/tasks/:id/commits.
func (s *Server) getTaskCommitsHandler(c echo.Context) error {
	taskID := c.Param("id")
	records, err := s.commit.GetTaskCommits(c.Request().Context(), taskID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, records)
}

// listBranchesHandler handles GET /api/v1/branches.
func (s *Server) listBranchesHandler(c echo.Context) error {
	branches, err := s.branch.ListBranches(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, branches)
}

// createBranchHandler handles POST /api/v1/branches.
func (s *Server) createBranchHandler(c echo.Context) error {
	var req struct {
		TaskID string `json:"task_id"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	branch, err := s.branch.CreateBranch(c.Request().Context(), req.TaskID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusCreated, branch)
}

// squashHandler handles POST /api/v1/squash.
func (s *Server) squashHandler(c echo.Context) error {
	var req service.SquashRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	commit, err := s.squash.SquashTask(c.Request().Context(), req)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusCreated, commit)
}

// rollbackHandler handles POST /api/v1/rollback.
func (s *Server) rollbackHandler(c echo.Context) error {
	var req service.RollbackRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	commit, err := s.rollback.RollbackTask(c.Request().Context(), req)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusCreated, commit)
}

// verifyHandler handles GET /api/v1/verify.
func (s *Server) verifyHandler(c echo.Context) error {
	issues, err := s.verify.VerifyIndexIntegrity(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"ok":     len(issues) == 0,
		"issues": issues,
	})
}

// ErrorResponse is the standard error response format.
// Refs: FR-9.1
type ErrorResponse struct {
	Error   string `json:"error"`
	Code    int    `json:"code"`
	Details string `json:"details,omitempty"`
}

// Ensure fmt is used
var _ = fmt.Sprintf
