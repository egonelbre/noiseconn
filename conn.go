package noiseconn

import (
	"encoding/binary"
	"errors"
	"io"
	"net"

	"github.com/flynn/noise"
	"github.com/zeebo/errs"
)

const HeaderByte = 0x80

// TODO(jt): this code is not 0-RTT for initial payloads larger than
// 65535 bytes! to my knowledge i don't know if this is actually a noise
// requirement, but is at least a github.com/flynn/noise requirement.

// TODO(jt): read and write cannot be called concurrently during handshake time

type Conn struct {
	net.Conn
	initiator        bool
	hs               *noise.HandshakeState
	hsResponsibility bool
	readMsgBuf       []byte
	writeMsgBuf      []byte
	readBuf          []byte
	send, recv       *noise.CipherState
}

var _ net.Conn = (*Conn)(nil)

// NewConn wraps an existing net.Conn with encryption provided by
// noise.Config.
func NewConn(conn net.Conn, config noise.Config) (*Conn, error) {
	hs, err := noise.NewHandshakeState(config)
	if err != nil {
		return nil, errs.Wrap(err)
	}
	return &Conn{
		Conn:             conn,
		hs:               hs,
		initiator:        config.Initiator,
		hsResponsibility: config.Initiator,
	}, nil
}

func (c *Conn) setCipherStates(cs1, cs2 *noise.CipherState) {
	if c.initiator {
		c.send, c.recv = cs1, cs2
	} else {
		c.send, c.recv = cs2, cs1
	}
}

func (c *Conn) hsRead() (err error) {
	c.readMsgBuf, err = c.readMsg(c.readMsgBuf[:0])
	if err != nil {
		return err
	}
	var cs1, cs2 *noise.CipherState
	c.readBuf, cs1, cs2, err = c.hs.ReadMessage(c.readBuf, c.readMsgBuf)
	if err != nil {
		return errs.Wrap(err)
	}
	c.setCipherStates(cs1, cs2)
	c.hsResponsibility = true
	if c.send != nil {
		c.hs = nil
	}
	return nil
}

func (c *Conn) Read(b []byte) (n int, err error) {
	handleBuffered := func() bool {
		if len(c.readBuf) == 0 {
			return false
		}
		n = copy(b, c.readBuf)
		copy(c.readBuf, c.readBuf[n:])
		c.readBuf = c.readBuf[:len(c.readBuf)-n]
		return true
	}

	if handleBuffered() {
		return n, nil
	}

	for c.hs != nil {
		if c.hsResponsibility {
			c.writeMsgBuf, err = c.hsCreate(c.writeMsgBuf[:0], nil)
			if err != nil {
				return 0, err
			}
			_, err = c.Conn.Write(c.writeMsgBuf)
			if err != nil {
				return 0, errs.Wrap(err)
			}
			if c.hs == nil {
				break
			}
		}
		err = c.hsRead()
		if err != nil {
			return 0, err
		}
		if handleBuffered() {
			return n, nil
		}
	}

	for {
		c.readMsgBuf, err = c.readMsg(c.readMsgBuf[:0])
		if err != nil {
			return 0, err
		}
		// TODO(jt): use b directly if b is big enough!
		// One option is to use b if it's big enough to
		// hold noise.MaxMsgLen, but another option that
		// would be neat is to figure out the payload size
		// from within m. it is also likely that
		// the payload size is never larger than the
		// message size and we could use that.
		c.readBuf, err = c.recv.Decrypt(c.readBuf, nil, c.readMsgBuf)
		if err != nil {
			return 0, errs.Wrap(err)
		}
		if handleBuffered() {
			return n, nil
		}
	}
}

// readMsg appends a message to b.
func (c *Conn) readMsg(b []byte) ([]byte, error) {
	// TODO(jt): make sure these reads are through bufio somewhere in the stack
	// appropriate.
	var msgHeader [4]byte
	_, err := io.ReadFull(c.Conn, msgHeader[:])
	if err != nil {
		return nil, errs.Wrap(err)
	}
	if msgHeader[0] != HeaderByte {
		// TODO(jt): close conn?
		return nil, errs.New("unknown message header")
	}
	msgHeader[0] = 0
	msgSize := int(binary.BigEndian.Uint32(msgHeader[:]))
	b = append(b[len(b):], make([]byte, msgSize)...)
	_, err = io.ReadFull(c.Conn, b)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errs.Wrap(io.ErrUnexpectedEOF)
		}
		return nil, errs.Wrap(err)
	}
	return b, nil
}

func (c *Conn) frame(header, b []byte) error {
	if len(b) >= 1<<(8*3) {
		return errs.New("message too large: %d", len(b))
	}
	binary.BigEndian.PutUint32(header[:4], uint32(len(b)))
	header[0] = HeaderByte
	return nil
}

func (c *Conn) hsCreate(out, payload []byte) (_ []byte, err error) {
	var cs1, cs2 *noise.CipherState
	outlen := len(out)
	out, cs1, cs2, err = c.hs.WriteMessage(append(out, make([]byte, 4)...), payload)
	if err != nil {
		return nil, errs.Wrap(err)
	}
	c.setCipherStates(cs1, cs2)
	c.hsResponsibility = false
	if c.send != nil {
		c.hs = nil
	}
	return out, c.frame(out[outlen:], out[outlen+4:])
}

func (c *Conn) writeHSPayload(b []byte) (sent bool, err error) {
	if c.hs != nil {
		c.writeMsgBuf, err = c.hsCreate(c.writeMsgBuf[:0], b)
		if err != nil {
			return false, err
		}
		_, err = c.Conn.Write(c.writeMsgBuf)
		return true, errs.Wrap(err)
	}
	return false, nil
}

// If a Noise handshake is still occurring (or has yet to occur), the
// data provided to Write will be included in handshake payloads. Note that
// even if the Noise configuration allows for 0-RTT, the request will only be
// 0-RTT if the request is 65535 bytes or smaller.
func (c *Conn) Write(b []byte) (n int, err error) {
	for c.hs != nil && len(b) > 0 {
		if !c.hsResponsibility {
			err = c.hsRead()
			if err != nil {
				return n, err
			}
		}
		if c.hs != nil {
			l := min(noise.MaxMsgLen, len(b))
			c.writeMsgBuf, err = c.hsCreate(c.writeMsgBuf[:0], b[:l])
			if err != nil {
				return n, err
			}
			_, err = c.Conn.Write(c.writeMsgBuf)
			if err != nil {
				return n, errs.Wrap(err)
			}
			n += l
			b = b[l:]
		}
	}

	c.writeMsgBuf = c.writeMsgBuf[:0]
	for len(b) > 0 {
		outlen := len(c.writeMsgBuf)
		l := min(noise.MaxMsgLen, len(b))
		c.writeMsgBuf, err = c.send.Encrypt(append(c.writeMsgBuf, make([]byte, 4)...), nil, b[:l])
		if err != nil {
			return n, errs.Wrap(err)
		}
		err = c.frame(c.writeMsgBuf[outlen:], c.writeMsgBuf[outlen+4:])
		if err != nil {
			return n, err
		}
		n += l
		b = b[l:]
	}
	_, err = c.Conn.Write(c.writeMsgBuf)
	return n, errs.Wrap(err)
}

// HandshakeComplete returns whether a handshake is complete.
func (c *Conn) HandshakeComplete() bool {
	return c.hs == nil
}

func min(a, b int) int {
	if a <= b {
		return a
	}
	return b
}
