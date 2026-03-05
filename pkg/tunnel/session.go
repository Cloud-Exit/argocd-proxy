package tunnel

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ConnectHandler is called on the agent side when the server requests a new
// connection through the tunnel.
type ConnectHandler func(session *Session, connID uint32, addr string)

// Session manages multiplexed connections over a single WebSocket.
type Session struct {
	ws      *websocket.Conn
	writeMu sync.Mutex

	conns   sync.Map     // uint32 -> *Conn
	nextID  atomic.Uint32
	closed  chan struct{}
	closeOnce sync.Once

	// OnConnect is called (agent side) for incoming connect requests.
	OnConnect ConnectHandler

	// handlerWg tracks active OnConnect goroutines for graceful draining.
	handlerWg sync.WaitGroup

	// connSem limits concurrent OnConnect handlers (nil = unlimited).
	connSem chan struct{}

	// lastPong stores the UnixNano timestamp of the last pong received.
	lastPong atomic.Int64

	log *slog.Logger
}

// DefaultMaxConns is the default maximum number of concurrent tunnel
// connections per session.
const DefaultMaxConns = 1024

// NewSession wraps a WebSocket connection into a tunnel session.
func NewSession(ws *websocket.Conn, logger *slog.Logger) *Session {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Session{
		ws:      ws,
		closed:  make(chan struct{}),
		connSem: make(chan struct{}, DefaultMaxConns),
		log:     logger,
	}
	s.lastPong.Store(time.Now().UnixNano())
	return s
}

// Serve runs the read loop, dispatching incoming messages to the appropriate
// connection. It blocks until the WebSocket is closed or the context is
// cancelled.
func (s *Session) Serve(ctx context.Context) error {
	defer func() { _ = s.Close() }()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Close the WebSocket when context is cancelled so ReadMessage unblocks.
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()

	// Ping loop.
	go s.pingLoop(ctx)

	for {
		_, data, err := s.ws.ReadMessage()
		if err != nil {
			select {
			case <-s.closed:
				return ctx.Err()
			default:
				return err
			}
		}

		msg, err := DecodeMessage(data)
		if err != nil {
			s.log.Warn("decode error", "err", err)
			continue
		}

		switch msg.Type {
		case MsgConnect:
			if s.OnConnect != nil {
				select {
				case s.connSem <- struct{}{}:
					s.handlerWg.Add(1)
					go func(id uint32, addr string) {
						defer func() { <-s.connSem }()
						defer s.handlerWg.Done()
						s.OnConnect(s, id, addr)
					}(msg.ConnID, string(msg.Data))
				default:
					s.log.Warn("max concurrent connections reached, rejecting", "connID", msg.ConnID)
					_ = s.SendError(msg.ConnID, "too many connections")
				}
			}

		case MsgConnected:
			if c := s.getConn(msg.ConnID); c != nil {
				c.setConnected()
			}

		case MsgData:
			if c := s.getConn(msg.ConnID); c != nil {
				if err := c.buf.write(msg.Data); err != nil {
					s.log.Warn("buffer overflow, closing conn", "connID", msg.ConnID)
					c.buf.close()
					s.removeConn(msg.ConnID)
				}
			}

		case MsgClose:
			if c := s.getConn(msg.ConnID); c != nil {
				c.buf.close()
				s.removeConn(msg.ConnID)
			}

		case MsgError:
			if c := s.getConn(msg.ConnID); c != nil {
				c.setError(string(msg.Data))
				c.buf.setErr(errStr(string(msg.Data)))
			}

		case MsgPing:
			_ = s.sendMsg(Message{Type: MsgPong})

		case MsgPong:
			s.lastPong.Store(time.Now().UnixNano())
		}
	}
}

// Dial opens a new tunnelled connection to the given address. The remote
// agent will TCP-dial the address and pipe data back through the tunnel.
func (s *Session) Dial(ctx context.Context, addr string) (*Conn, error) {
	select {
	case <-s.closed:
		return nil, ErrSessionClosed
	default:
	}

	id := s.nextID.Add(1)
	c := newConn(s, id)
	s.conns.Store(id, c)

	if err := s.sendMsg(Message{Type: MsgConnect, ConnID: id, Data: []byte(addr)}); err != nil {
		s.removeConn(id)
		return nil, err
	}

	// Wait for the agent to confirm or reject.
	timeout := 30 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		timeout = time.Until(dl)
	}
	if err := c.waitConnected(timeout); err != nil {
		s.removeConn(id)
		return nil, err
	}
	return c, nil
}

// Accept registers a conn created by the remote (agent) side, returns it so
// the agent handler can start piping data.
func (s *Session) Accept(connID uint32) *Conn {
	c := newConn(s, connID)
	close(c.connected) // immediately connected
	s.conns.Store(connID, c)
	return c
}

// Drain stops accepting new connections and waits up to timeout for active
// OnConnect handlers to finish.
func (s *Session) Drain(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		s.handlerWg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		s.log.Warn("drain timed out, forcing close")
	}
}

// Close tears down the session and all open connections.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.conns.Range(func(key, val any) bool {
			if c, ok := val.(*Conn); ok {
				c.buf.close()
			}
			s.conns.Delete(key)
			return true
		})
		_ = s.ws.Close()
	})
	return nil
}

// Done returns a channel that is closed when the session is torn down.
func (s *Session) Done() <-chan struct{} { return s.closed }

func (s *Session) sendMsg(m Message) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	select {
	case <-s.closed:
		return ErrSessionClosed
	default:
	}
	return s.ws.WriteMessage(websocket.BinaryMessage, m.Encode())
}

func (s *Session) getConn(id uint32) *Conn {
	v, ok := s.conns.Load(id)
	if !ok {
		return nil
	}
	return v.(*Conn)
}

func (s *Session) removeConn(id uint32) { s.conns.Delete(id) }

// SendConnected sends a MsgConnected for the given connection ID. Used by
// agents after successfully dialling the local target.
func (s *Session) SendConnected(connID uint32) error {
	return s.sendMsg(Message{Type: MsgConnected, ConnID: connID})
}

// SendError sends a MsgError for the given connection ID.
func (s *Session) SendError(connID uint32, msg string) error {
	return s.sendMsg(Message{Type: MsgError, ConnID: connID, Data: []byte(msg)})
}

const (
	pingInterval = 15 * time.Second
	pongTimeout  = 45 * time.Second // 3x ping interval
)

func (s *Session) pingLoop(ctx context.Context) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case <-t.C:
			// Detect dead peers: if no pong received within pongTimeout,
			// close the session.
			last := time.Unix(0, s.lastPong.Load())
			if time.Since(last) > pongTimeout {
				s.log.Warn("pong timeout, closing session", "last_pong", last)
				_ = s.Close()
				return
			}
			if err := s.sendMsg(Message{Type: MsgPing}); err != nil {
				return
			}
		}
	}
}
