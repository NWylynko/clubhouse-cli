package sessionnet

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

const (
	handshakeInfo   = "clubhouse-sessionnet-v1"
	maxRecord       = 16 * 1024
	handshakeWindow = 20 * time.Second
)

var framesWritten atomic.Int64

func RecordsWritten() int64 { return framesWritten.Load() }

var errHandshake = errors.New("sessionnet: handshake failed — wrong session key, or not a session peer")

type secureConn struct {
	net.Conn
	sealer interface {
		Seal(dst, nonce, plaintext, additionalData []byte) []byte
	}
	opener interface {
		Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
	}
	sendCtr, recvCtr uint64
	buf              []byte
	hdr              [2]byte
}

func nonceFor(ctr uint64) []byte {
	var n [chacha20poly1305.NonceSize]byte
	binary.BigEndian.PutUint64(n[4:], ctr)
	return n[:]
}

func (c *secureConn) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxRecord {
			chunk = chunk[:maxRecord]
		}
		frame := make([]byte, 2, 2+len(chunk)+chacha20poly1305.Overhead)
		frame = c.sealer.Seal(frame, nonceFor(c.sendCtr), chunk, nil)
		c.sendCtr++
		binary.BigEndian.PutUint16(frame[:2], uint16(len(frame)-2))
		framesWritten.Add(1)
		if _, err := c.Conn.Write(frame); err != nil {
			return written, err
		}
		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}

func (c *secureConn) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		if _, err := io.ReadFull(c.Conn, c.hdr[:]); err != nil {
			return 0, err
		}
		n := int(binary.BigEndian.Uint16(c.hdr[:]))
		if n == 0 || n > maxRecord+chacha20poly1305.Overhead {
			return 0, fmt.Errorf("sessionnet: bad record length %d", n)
		}
		ct := make([]byte, n)
		if _, err := io.ReadFull(c.Conn, ct); err != nil {
			return 0, err
		}
		pt, err := c.opener.Open(nil, nonceFor(c.recvCtr), ct, nil)
		if err != nil {
			return 0, fmt.Errorf("sessionnet: record authentication failed: %w", err)
		}
		c.recvCtr++
		c.buf = pt
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func handshake(raw net.Conn, key Key, initiator bool) (net.Conn, error) {
	if key.Zero() {
		return nil, fmt.Errorf("sessionnet: no session key — this member was never admitted to the session")
	}
	_ = raw.SetDeadline(time.Now().Add(handshakeWindow))

	var priv [32]byte
	if _, err := rand.Read(priv[:]); err != nil {
		return nil, err
	}
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	peer := make([]byte, 32)
	if initiator {
		if _, err := raw.Write(pub); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(raw, peer); err != nil {
			return nil, fmt.Errorf("%w: %v", errHandshake, err)
		}
	} else {
		if _, err := io.ReadFull(raw, peer); err != nil {
			return nil, fmt.Errorf("%w: %v", errHandshake, err)
		}
		if _, err := raw.Write(pub); err != nil {
			return nil, err
		}
	}
	shared, err := curve25519.X25519(priv[:], peer)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errHandshake, err)
	}

	ci, cr := pub, peer
	if !initiator {
		ci, cr = peer, pub
	}
	info := append([]byte(handshakeInfo), append(ci, cr...)...)
	kdf := hkdf.New(sha256.New, shared, key[:], info)
	keys := make([]byte, 64)
	if _, err := io.ReadFull(kdf, keys); err != nil {
		return nil, err
	}
	i2r, r2i := keys[:32], keys[32:]
	sendKey, recvKey := i2r, r2i
	if !initiator {
		sendKey, recvKey = r2i, i2r
	}
	sealer, err := chacha20poly1305.New(sendKey)
	if err != nil {
		return nil, err
	}
	opener, err := chacha20poly1305.New(recvKey)
	if err != nil {
		return nil, err
	}
	c := &secureConn{Conn: raw, sealer: sealer, opener: opener}

	confirm := []byte(handshakeInfo + "-confirm")
	if _, err := c.Write(confirm); err != nil {
		return nil, err
	}
	got := make([]byte, len(confirm))
	if _, err := io.ReadFull(c, got); err != nil {
		return nil, fmt.Errorf("%w: %v", errHandshake, err)
	}
	if string(got) != string(confirm) {
		return nil, errHandshake
	}
	_ = raw.SetDeadline(time.Time{})
	return c, nil
}
