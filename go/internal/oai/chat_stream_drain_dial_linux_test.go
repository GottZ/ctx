package oai_test

import (
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

// startDrainClient is startSlowClientDial with a client whose receive buffer
// is 4 KiB from the connect on (SO_RCVBUF before connect(2)): the first
// window it announces is small, so what the client has not read stays in the
// route's writer — not in a window announced before the buffer shrank — and
// a client that reads on gets each window update at once (no zero-window
// probe of the sender in between). The drain gates of the A03-W10 landing
// count the chunks the route writes after a revocation and time its error
// frame with it.
func startDrainClient(t *testing.T, h http.Handler, hdr ...string) *slowClient {
	t.Helper()
	return startSlowClientDial(t, h, func(addr string) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second, Control: func(_, _ string, rc syscall.RawConn) error {
			var serr error
			if err := rc.Control(func(fd uintptr) {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
			}); err != nil {
				return err
			}
			return serr
		}}
		return d.Dial("tcp", addr)
	}, hdr...)
}
