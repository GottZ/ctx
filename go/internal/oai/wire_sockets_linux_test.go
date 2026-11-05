//go:build linux

package oai_test

// Deterministic transport faults for the wire gates of the meter (W06-10
// hardening 3): a port no other socket can take, a listener that stops
// listening and keeps its port (every later connect is refused), and a
// listener that drops every further SYN (a connect that hangs until its
// deadline). Fremd-Review 4 F4-3: a closed httptest server's port came back
// to the next listener of the same test, so the "dead" row answered.

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"strconv"
	"syscall"
	"testing"
)

// heldListener is a TCP listener on 127.0.0.1 that holds its port for the
// whole test, also after stop: the socket clears SO_REUSEADDR and binds an
// explicit port (Linux keeps an explicitly bound port when the socket
// returns to the closed state; a port picked by bind(0) goes back to the
// pool there — and to the next listener of any process on the host). No
// other socket can bind the port, and the kernel never hands it out as an
// ephemeral one. The port comes from [20000, 32768), below the default
// ephemeral range; a port in use is skipped.
type heldListener struct {
	net.Listener
	t    testing.TB
	keep net.Conn // dropSYNs: the one connection that fills the accept queue
}

func newHeldListener(t testing.TB) *heldListener {
	t.Helper()
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 0)
		}); err != nil {
			return err
		}
		return serr
	}}
	var ln net.Listener
	var err error
	for range 200 {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(20000+rand.IntN(12768)))
		if ln, err = lc.Listen(context.Background(), "tcp4", addr); err == nil || !errors.Is(err, syscall.EADDRINUSE) {
			break
		}
	}
	if err != nil {
		t.Fatalf("held listener: %v", err)
	}
	l := &heldListener{Listener: ln, t: t}
	t.Cleanup(func() {
		if l.keep != nil {
			_ = l.keep.Close()
		}
		_ = ln.Close()
	})
	return l
}

// control runs f on the listening socket.
func (l *heldListener) control(what string, f func(fd int) error) {
	l.t.Helper()
	rc, err := l.Listener.(*net.TCPListener).SyscallConn()
	if err != nil {
		l.t.Fatalf("%s: %v", what, err)
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = f(int(fd)) }); err != nil {
		l.t.Fatalf("%s: %v", what, err)
	}
	if ferr != nil {
		l.t.Fatalf("%s: %v", what, ferr)
	}
}

// stop ends listening and keeps the port: shutdown(SHUT_RD) returns a
// listening socket to the closed state, still bound to its explicit port.
// From then on every connect to the address is refused, and a pending
// Accept returns an error.
func (l *heldListener) stop() {
	l.t.Helper()
	l.control("stop listening", func(fd int) error { return syscall.Shutdown(fd, syscall.SHUT_RD) })
}

// dropSYNs leaves the accept queue room for one connection (listen backlog
// 0) that nobody accepts, and fills it: Linux drops every further SYN, so a
// connect hangs until its deadline — a host behind a dropping firewall, on
// loopback. Nobody may call Accept on l afterwards.
func (l *heldListener) dropSYNs() {
	l.t.Helper()
	l.control("backlog 0", func(fd int) error { return syscall.Listen(fd, 0) })
	c, err := net.Dial("tcp4", l.Addr().String())
	if err != nil {
		l.t.Fatalf("filling the accept queue: %v", err)
	}
	l.keep = c
}

// refusedAddr is a 127.0.0.1 address whose every connect is refused for the
// rest of the test: a held listener, stopped.
func refusedAddr(t testing.TB) string {
	t.Helper()
	l := newHeldListener(t)
	l.stop()
	return l.Addr().String()
}
