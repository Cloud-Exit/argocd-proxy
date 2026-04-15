package tunnel

import "net"

type closeWriter interface {
	CloseWrite() error
}

type closeReader interface {
	CloseRead() error
}

// CloseWrite closes the write side of conn when supported, otherwise it closes
// the whole connection.
func CloseWrite(conn net.Conn) {
	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = conn.Close()
}

// CloseRead closes the read side of conn when supported, otherwise it closes
// the whole connection.
func CloseRead(conn net.Conn) {
	if cr, ok := conn.(closeReader); ok {
		_ = cr.CloseRead()
		return
	}
	_ = conn.Close()
}
