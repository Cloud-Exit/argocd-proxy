package tunnel

import "errors"

var (
	ErrShortMessage    = errors.New("tunnel: message too short")
	ErrSessionClosed   = errors.New("tunnel: session closed")
	ErrConnClosed      = errors.New("tunnel: connection closed")
	ErrConnectFailed   = errors.New("tunnel: connect failed")
	ErrConnectTimeout  = errors.New("tunnel: connect timeout")
	ErrUnknownConn     = errors.New("tunnel: unknown connection id")
	ErrBufferFull      = errors.New("tunnel: read buffer full")
)
