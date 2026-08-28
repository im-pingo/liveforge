package sipclose

import (
	"errors"
	"net"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

const connectionCloseWait = time.Second

// CloseUserAgent lets sipgo's transport readers release their references before
// UserAgent.Close clears the transport pool. sipgo's pool hard-close path sets
// a connection reference to zero, while the reader's deferred cleanup still
// decrements it once more.
func CloseUserAgent(ua *sipgo.UserAgent, connections []sip.Connection) error {
	if ua == nil {
		return nil
	}

	var closeErr error
	for _, connection := range connections {
		// Keep one reference for the reader itself. Every other reference is
		// owned by a completed request or the transport idle slot; releasing
		// those first lets the reader's deferred cleanup reach zero without
		// calling Close on an already closed socket.
		for connection != nil && connection.Ref(0) > 1 {
			connection.Ref(-1)
		}
		switch connection := connection.(type) {
		case *sip.UDPConnection:
			if connection.PacketConn != nil {
				if err := connection.PacketConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					closeErr = errors.Join(closeErr, err)
				}
			}
		case *sip.TCPConnection:
			if connection.Conn != nil {
				if err := connection.Conn.Close(); err != nil {
					closeErr = errors.Join(closeErr, err)
				}
			}
		}
	}

	deadline := time.NewTimer(connectionCloseWait)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		closed := true
		for _, connection := range connections {
			if connection != nil && connection.Ref(0) > 0 {
				closed = false
				break
			}
		}
		if closed {
			break
		}
		select {
		case <-deadline.C:
			closeErr = errors.Join(closeErr, errors.New("timed out waiting for SIP transport readers to close"))
			closed = true
		case <-ticker.C:
		}
		if closed {
			break
		}
	}

	return errors.Join(closeErr, ua.Close())
}
