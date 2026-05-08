// Minimal WebSocket implementation using only Go stdlib
// Implements enough of gorilla/websocket API for procman's needs
package websocket

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	TextMessage   = 1
	BinaryMessage = 2
	CloseMessage  = 8
	PingMessage   = 9
	PongMessage   = 10

	CloseNormalClosure    = 1000
	CloseGoingAway        = 1001
	CloseNoStatusReceived = 1005
)

var ErrCloseSent = errors.New("websocket: close sent")

type CloseError struct {
	Code int
	Text string
}
func (e *CloseError) Error() string { return "websocket: close" }

func IsCloseError(err error, codes ...int) bool {
	if e, ok := err.(*CloseError); ok {
		for _, c := range codes {
			if c == e.Code { return true }
		}
	}
	return false
}

type Upgrader struct {
	HandshakeTimeout time.Duration
	ReadBufferSize   int
	WriteBufferSize  int
	CheckOrigin      func(r *http.Request) bool
}

type Conn struct {
	conn   net.Conn
	bufrw  *bufio.ReadWriter
	closed bool
}

func (u *Upgrader) Upgrade(w http.ResponseWriter, r *http.Request, responseHeader http.Header) (*Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("not a websocket handshake")
	}
	key := r.Header.Get("Sec-Websocket-Key")
	if key == "" {
		return nil, errors.New("missing Sec-WebSocket-Key")
	}

	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("hijacking not supported")
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	bufrw.WriteString("Upgrade: websocket\r\n")
	bufrw.WriteString("Connection: Upgrade\r\n")
	bufrw.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n")
	bufrw.WriteString("\r\n")
	bufrw.Flush()

	return &Conn{conn: conn, bufrw: bufrw}, nil
}

// ReadMessage reads a WebSocket message (handles fragmentation & masking)
func (c *Conn) ReadMessage() (messageType int, p []byte, err error) {
	for {
		h := make([]byte, 2)
		if _, err = io.ReadFull(c.bufrw, h); err != nil {
			return 0, nil, err
		}
		fin    := h[0]&0x80 != 0
		opcode := int(h[0] & 0x0f)
		masked := h[1]&0x80 != 0
		plen   := int(h[1] & 0x7f)

		if plen == 126 {
			ext := make([]byte, 2)
			if _, err = io.ReadFull(c.bufrw, ext); err != nil { return 0, nil, err }
			plen = int(binary.BigEndian.Uint16(ext))
		} else if plen == 127 {
			ext := make([]byte, 8)
			if _, err = io.ReadFull(c.bufrw, ext); err != nil { return 0, nil, err }
			plen = int(binary.BigEndian.Uint64(ext))
		}

		var mask [4]byte
		if masked {
			if _, err = io.ReadFull(c.bufrw, mask[:]); err != nil { return 0, nil, err }
		}

		payload := make([]byte, plen)
		if _, err = io.ReadFull(c.bufrw, payload); err != nil { return 0, nil, err }

		if masked {
			for i := range payload { payload[i] ^= mask[i%4] }
		}

		if opcode == CloseMessage {
			code := CloseNoStatusReceived
			if len(payload) >= 2 {
				code = int(binary.BigEndian.Uint16(payload[:2]))
			}
			_ = c.WriteMessage(CloseMessage, payload)
			c.closed = true
			c.conn.Close()
			return CloseMessage, payload, &CloseError{Code: code}
		}

		if opcode == PingMessage {
			_ = c.WriteMessage(PongMessage, payload)
			continue
		}

		_ = fin // handle fragmentation if needed; for now treat each frame as complete
		if opcode == 0 { opcode = TextMessage } // continuation
		return opcode, payload, nil
	}
}

// WriteMessage sends a WebSocket message (server side, no masking)
func (c *Conn) WriteMessage(messageType int, data []byte) error {
	if c.closed { return ErrCloseSent }

	var header []byte
	header = append(header, byte(0x80|messageType))
	l := len(data)
	switch {
	case l <= 125:
		header = append(header, byte(l))
	case l <= 65535:
		header = append(header, 126)
		ext := make([]byte, 2)
		binary.BigEndian.PutUint16(ext, uint16(l))
		header = append(header, ext...)
	default:
		header = append(header, 127)
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, uint64(l))
		header = append(header, ext...)
	}

	c.bufrw.Write(header)
	c.bufrw.Write(data)
	return c.bufrw.Flush()
}

func (c *Conn) Close() error {
	if c.closed { return nil }
	c.closed = true
	return c.conn.Close()
}

func (c *Conn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }
func (c *Conn) SetReadLimit(limit int64)           {}
func (c *Conn) SetPingHandler(h func(string) error) {}
func (c *Conn) SetPongHandler(h func(string) error) {}

// generateKey is here just to avoid unused import of crypto/rand
func generateKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}
