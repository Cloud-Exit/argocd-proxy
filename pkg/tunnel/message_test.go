package tunnel

import (
	"bytes"
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
	}{
		{"ping", Message{Type: MsgPing, ConnID: 0}},
		{"pong", Message{Type: MsgPong, ConnID: 0}},
		{"connect", Message{Type: MsgConnect, ConnID: 42, Data: []byte("10.0.0.1:443")}},
		{"connected", Message{Type: MsgConnected, ConnID: 42}},
		{"data", Message{Type: MsgData, ConnID: 7, Data: []byte("hello world")}},
		{"close", Message{Type: MsgClose, ConnID: 7}},
		{"error", Message{Type: MsgError, ConnID: 7, Data: []byte("connection refused")}},
		{"empty data", Message{Type: MsgData, ConnID: 1, Data: []byte{}}},
		{"large payload", Message{Type: MsgData, ConnID: 99, Data: bytes.Repeat([]byte("x"), 64*1024)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := tt.msg.Encode()
			got, err := DecodeMessage(encoded)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Type != tt.msg.Type {
				t.Errorf("type: got %d, want %d", got.Type, tt.msg.Type)
			}
			if got.ConnID != tt.msg.ConnID {
				t.Errorf("connID: got %d, want %d", got.ConnID, tt.msg.ConnID)
			}
			if !bytes.Equal(got.Data, tt.msg.Data) {
				t.Errorf("data: got %d bytes, want %d bytes", len(got.Data), len(tt.msg.Data))
			}
		})
	}
}

func TestDecodeShortMessage(t *testing.T) {
	_, err := DecodeMessage([]byte{0x01, 0x02})
	if err != ErrShortMessage {
		t.Errorf("got %v, want ErrShortMessage", err)
	}
}

func TestEncodeLength(t *testing.T) {
	msg := Message{Type: MsgData, ConnID: 1, Data: []byte("abc")}
	encoded := msg.Encode()
	if len(encoded) != headerSize+3 {
		t.Errorf("length: got %d, want %d", len(encoded), headerSize+3)
	}
}
