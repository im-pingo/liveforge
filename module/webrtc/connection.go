package webrtc

import (
	"sync"

	"github.com/im-pingo/liveforge/core"
)

// newConnectionRelease returns an idempotent release function for one
// acquired server connection. HTTP error paths and ICE callbacks can race;
// sync.Once keeps the accounting balanced in both cases.
func newConnectionRelease(server *core.Server) func() {
	var once sync.Once
	return func() {
		once.Do(server.ReleaseConn)
	}
}
