//go:build unix

package libkrun

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

// The two raw-descriptor primitives the lifeline's provenance check needs.
// They are raw on purpose: both run BEFORE the descriptor has been proven to
// belong to this child, and wrapping an unproven descriptor in an os.File is
// exactly the defect they exist to prevent — os.File attaches a finalizer, so
// a wrong guess gets closed by the garbage collector, which is how the first
// attempt at MGIT-103 killed the Go runtime's netpoller.
//
// Neither primitive MUTATES the descriptor. A rejected descriptor belongs to
// somebody else — very possibly the runtime — and must come back exactly as it
// was found: not closed, not reflagged, not registered with the poller.
// Refs: MGIT-103

// fdIsPipe reports whether fd is a pipe (FIFO). It is the cheap structural
// gate, and the one that matters most: on Linux the descriptor number a VM
// child is told to use is commonly the runtime's epoll descriptor, whose fstat
// reports no file type at all, so this rejects it — as it rejects sockets,
// regular files, terminals and closed descriptors.
func fdIsPipe(fd int) bool {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return false
	}
	return st.Mode&syscall.S_IFMT == syscall.S_IFIFO
}

// readFDExactly reads exactly n bytes from fd without wrapping it in an
// os.File, giving up after timeout.
//
// The timeout is not for the real case — the daemon writes the nonce before
// exec, so a genuine child finds it already buffered and never waits — but so
// that a stray descriptor which happens to be an idle pipe is REJECTED rather
// than left hanging a VM's boot. Bounding it in a goroutine rather than by
// setting O_NONBLOCK is deliberate: flipping flags on a descriptor that turns
// out to belong to someone else is a smaller version of the same mistake this
// whole check exists to stop. In the pathological case the reader stays parked
// on a descriptor nothing else will ever use, which costs one thread in a
// process that is about to be refused its lifeline anyway.
func readFDExactly(fd, n int, timeout time.Duration) ([]byte, error) {
	type result struct {
		buf []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		buf := make([]byte, n)
		for filled := 0; filled < n; {
			got, err := syscall.Read(fd, buf[filled:])
			switch {
			case err == nil && got == 0:
				done <- result{err: errors.New("lifeline closed before it identified itself")}
				return
			case err == nil:
				filled += got
			case errors.Is(err, syscall.EINTR):
				continue
			default:
				done <- result{err: fmt.Errorf("lifeline read: %w", err)}
				return
			}
		}
		done <- result{buf: buf}
	}()

	select {
	case r := <-done:
		return r.buf, r.err
	case <-time.After(timeout):
		return nil, errors.New("lifeline never identified itself")
	}
}
