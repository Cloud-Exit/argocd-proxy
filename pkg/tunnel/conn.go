package tunnel

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// maxReadBufferSize is the maximum amount of data that can be buffered in a
// single tunnel connection before writes are rejected. [H1 fix]
const maxReadBufferSize = 64 << 20 // 64 MiB

// Conn implements net.Conn over a multiplexed tunnel session.
type Conn struct {
	session *Session
	connID  uint32
	buf     readBuffer

	connected chan struct{} // closed when peer confirms connection
	connErr   error        // set if connect failed

	done      chan struct{}
	closeOnce sync.Once
}

func newConn(s *Session, id uint32) *Conn {
	c := &Conn{
		session:   s,
		connID:    id,
		connected: make(chan struct{}),
		done:      make(chan struct{}),
	}
	c.buf.cond = sync.NewCond(&c.buf.mu)
	return c
}

// waitConnected blocks until the peer confirms the connection or returns an
// error if the connection fails or the context expires.
func (c *Conn) waitConnected(timeout time.Duration) error {
	select {
	case <-c.connected:
		return c.connErr
	case <-c.done:
		return ErrConnClosed
	case <-time.After(timeout):
		return ErrConnectTimeout
	}
}

func (c *Conn) setConnected()       { close(c.connected) }
func (c *Conn) setError(msg string) { c.connErr = errors.Join(ErrConnectFailed, errStr(msg)); close(c.connected) }

func (c *Conn) Read(p []byte) (int, error) {
	return c.buf.read(p)
}

func (c *Conn) Write(p []byte) (int, error) {
	select {
	case <-c.done:
		return 0, ErrConnClosed
	default:
	}
	// Copy to avoid caller mutating the slice before the write completes.
	data := make([]byte, len(p))
	copy(data, p)
	if err := c.session.sendMsg(Message{Type: MsgData, ConnID: c.connID, Data: data}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.buf.close()
		// Best-effort notify the peer.
		_ = c.session.sendMsg(Message{Type: MsgClose, ConnID: c.connID})
		c.session.removeConn(c.connID)
	})
	return nil
}

func (c *Conn) LocalAddr() net.Addr                { return tunnelAddr{} }
func (c *Conn) RemoteAddr() net.Addr               { return tunnelAddr{} }
func (c *Conn) SetDeadline(_ time.Time) error      { return nil }
func (c *Conn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *Conn) SetWriteDeadline(_ time.Time) error { return nil }

type tunnelAddr struct{}

func (tunnelAddr) Network() string { return "tunnel" }
func (tunnelAddr) String() string  { return "tunnel" }

// errStr wraps a string as an error.
type errStr string

func (e errStr) Error() string { return string(e) }

// ---------- readBuffer ----------

// readBuffer is a blocking FIFO buffer fed by the session read loop and
// consumed by Conn.Read. It enforces a maximum size to prevent OOM. [H1 fix]
type readBuffer struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
	err    error
}

func (rb *readBuffer) write(p []byte) error {
	rb.mu.Lock()
	if rb.closed {
		rb.mu.Unlock()
		return ErrConnClosed
	}
	if len(rb.buf)+len(p) > maxReadBufferSize {
		rb.mu.Unlock()
		return ErrBufferFull
	}
	rb.buf = append(rb.buf, p...)
	rb.mu.Unlock()
	rb.cond.Signal()
	return nil
}

func (rb *readBuffer) setErr(err error) {
	rb.mu.Lock()
	rb.err = err
	rb.closed = true
	rb.mu.Unlock()
	rb.cond.Broadcast()
}

func (rb *readBuffer) read(p []byte) (int, error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	for len(rb.buf) == 0 {
		if rb.closed {
			if rb.err != nil {
				return 0, rb.err
			}
			return 0, io.EOF
		}
		rb.cond.Wait()
	}
	n := copy(p, rb.buf)
	rb.buf = rb.buf[n:]
	return n, nil
}

func (rb *readBuffer) close() {
	rb.mu.Lock()
	rb.closed = true
	rb.mu.Unlock()
	rb.cond.Broadcast()
}
