//go:build linux

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	"github.com/hyper-swe/mgit/internal/guest"
)

// serveGuest performs PID-1 duties — mount the worktree and a tmpfs
// /tmp — then accepts exec connections over vsock, serving each with
// the supervisor. One connection is one exec request (FR-17.11).
func serveGuest(ctx context.Context, supervisor *guest.Supervisor, port uint32, logger *slog.Logger) error {
	if err := mountGuestFilesystems(); err != nil {
		return err
	}

	listener, err := vsock.Listen(port, nil)
	if err != nil {
		return fmt.Errorf("mgit-guest: vsock listen :%d: %w", port, err)
	}
	defer func() { _ = listener.Close() }()

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	logger.Info("mgit-guest serving", "event", "started", "vsock_port", port)
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("mgit-guest: accept: %w", err)
			}
		}
		go serveConn(ctx, supervisor, conn, logger)
	}
}

// serveConn serves one exec connection and closes it.
func serveConn(ctx context.Context, supervisor *guest.Supervisor, conn net.Conn, logger *slog.Logger) {
	defer func() { _ = conn.Close() }()
	if err := supervisor.Serve(ctx, conn); err != nil {
		logger.Error("mgit-guest exec failed", "event", "exec_error", "error", err.Error())
	}
}

// mountGuestFilesystems mounts the worktree share at /work (virtiofs,
// identical-path per FR-17.3) and a tmpfs at /tmp. Idempotent enough
// for boot: an already-mounted target is tolerated.
func mountGuestFilesystems() error {
	if err := unix.Mount("work", "/work", "virtiofs", 0, ""); err != nil && err != unix.EBUSY {
		return fmt.Errorf("mgit-guest: mount /work: %w", err)
	}
	if err := unix.Mount("tmpfs", "/tmp", "tmpfs", 0, ""); err != nil && err != unix.EBUSY {
		return fmt.Errorf("mgit-guest: mount /tmp: %w", err)
	}
	return nil
}
