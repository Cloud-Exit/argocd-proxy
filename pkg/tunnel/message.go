// Package tunnel implements a multiplexed TCP tunnel over WebSocket.
package tunnel

import "encoding/binary"

// Message types for the tunnel protocol.
const (
	MsgConnect   byte = 0x01 // Server->Agent: dial target address (payload = addr string).
	MsgConnected byte = 0x02 // Agent->Server: connection established.
	MsgData      byte = 0x03 // Bidirectional: raw bytes.
	MsgError     byte = 0x04 // Bidirectional: error string.
	MsgClose     byte = 0x05 // Bidirectional: close connection.
	MsgPing      byte = 0x06 // Keepalive request (connID=0).
	MsgPong      byte = 0x07 // Keepalive response (connID=0).
)

// Message is a tunnel protocol frame.
//
// Wire format: [type:1][connID:4 big-endian][payload:...]
type Message struct {
	Type   byte
	ConnID uint32
	Data   []byte
}

const headerSize = 5

// Encode serialises the message into a byte slice suitable for a WebSocket
// binary frame.
func (m Message) Encode() []byte {
	buf := make([]byte, headerSize+len(m.Data))
	buf[0] = m.Type
	binary.BigEndian.PutUint32(buf[1:5], m.ConnID)
	copy(buf[headerSize:], m.Data)
	return buf
}

// DecodeMessage parses a binary frame into a Message. It returns an error if
// the payload is too short.
func DecodeMessage(b []byte) (Message, error) {
	if len(b) < headerSize {
		return Message{}, ErrShortMessage
	}
	return Message{
		Type:   b[0],
		ConnID: binary.BigEndian.Uint32(b[1:5]),
		Data:   b[headerSize:],
	}, nil
}
