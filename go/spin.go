// A connection reader that briefly spins on a non-blocking read before parking in the netpoller.
//
// A sequential client (one request, wait for the answer, next request) leaves the socket empty for a
// few hundred microseconds between requests. Parking the goroutine in epoll and waking it again costs
// several syscalls and a thread hand-off each time, which is slow under a sandboxed kernel. Polling the
// socket for a short window first usually finds the next request already there. Only one connection
// spins at a time, so concurrent workloads never burn more than one CPU on it.

package main

import (
	"io"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

var spinWindow = func() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("FORGE_SPIN_US")); err == nil {
		return time.Duration(v) * time.Microsecond
	}
	return 60 * time.Microsecond
}()

var spinners atomic.Int32

type spinReader struct {
	rc syscall.RawConn
}

func newConnReader(conn net.Conn) io.Reader {
	if spinWindow <= 0 {
		return conn
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		if rc, err := tc.SyscallConn(); err == nil {
			return &spinReader{rc: rc}
		}
	}
	return conn
}

func (r *spinReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var n int
	var rerr error
	spin := spinners.Add(1) == 1
	if !spin {
		spinners.Add(-1)
	}
	var deadline time.Time
	if spin {
		deadline = time.Now().Add(spinWindow)
	}
	err := r.rc.Read(func(fd uintptr) bool {
		for {
			m, e := syscall.Read(int(fd), p)
			if e == syscall.EINTR {
				continue
			}
			if e == syscall.EAGAIN {
				if spin && time.Now().Before(deadline) {
					continue
				}
				if spin {
					spin = false
					spinners.Add(-1)
				}
				return false
			}
			if e != nil {
				n, rerr = 0, e
			} else if m == 0 {
				n, rerr = 0, io.EOF
			} else {
				n = m
			}
			return true
		}
	})
	if spin {
		spinners.Add(-1)
	}
	if err != nil {
		return 0, err
	}
	return n, rerr
}
