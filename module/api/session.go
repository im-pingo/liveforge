package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/config"
)

type sessionManager struct {
	secret     [32]byte
	generation atomic.Uint64
}

func newSessionManager() (*sessionManager, error) {
	sessions := &sessionManager{}
	if _, err := rand.Read(sessions.secret[:]); err != nil {
		return nil, fmt.Errorf("generate session secret: %w", err)
	}
	sessions.generation.Store(1)
	return sessions, nil
}

func mustNewSessionManager() *sessionManager {
	sessions, err := newSessionManager()
	if err != nil {
		panic(err)
	}
	return sessions
}

func (s *sessionManager) generationSnapshot() uint64 {
	return s.generation.Load()
}

func (s *sessionManager) generateTokenFor(expectedGeneration uint64, cfg config.ConsoleConfig) (string, bool) {
	if s.generation.Load() != expectedGeneration {
		return "", false
	}
	fingerprint := s.credentialFingerprint(cfg)
	payload := fmt.Sprintf("%x.%x.%s", expectedGeneration, time.Now().Add(24*time.Hour).Unix(), hex.EncodeToString(fingerprint[:]))
	mac := hmac.New(sha256.New, s.secret[:])
	mac.Write([]byte("session-token-v1:"))
	mac.Write([]byte(payload))
	token := payload + "." + hex.EncodeToString(mac.Sum(nil))
	if s.generation.Load() != expectedGeneration {
		return "", false
	}
	return token, true
}

func (s *sessionManager) validate(r *http.Request, cfg config.ConsoleConfig) bool {
	cookie, err := r.Cookie("lf_session")
	if err != nil {
		return false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 4 {
		return false
	}
	generation, err := strconv.ParseUint(parts[0], 16, 64)
	if err != nil || generation != s.generation.Load() {
		return false
	}
	expires, err := strconv.ParseInt(parts[1], 16, 64)
	if err != nil || time.Now().Unix() >= expires {
		return false
	}
	fingerprint, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	expectedFingerprint := s.credentialFingerprint(cfg)
	if !hmac.Equal(fingerprint, expectedFingerprint[:]) {
		return false
	}
	payload := parts[0] + "." + parts[1] + "." + parts[2]
	mac := hmac.New(sha256.New, s.secret[:])
	mac.Write([]byte("session-token-v1:"))
	mac.Write([]byte(payload))
	expected, err := hex.DecodeString(parts[3])
	return err == nil && hmac.Equal(mac.Sum(nil), expected)
}

func (s *sessionManager) credentialFingerprint(cfg config.ConsoleConfig) [sha256.Size]byte {
	data := make([]byte, 0, len(cfg.Username)+len(cfg.Password)+len(cfg.PasswordHash)+24)
	for _, value := range []string{cfg.Username, cfg.Password, cfg.PasswordHash} {
		data = binary.BigEndian.AppendUint64(data, uint64(len(value)))
		data = append(data, value...)
	}
	mac := hmac.New(sha256.New, s.secret[:])
	mac.Write([]byte("console-credentials-v1:"))
	mac.Write(data)
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], mac.Sum(nil))
	return fingerprint
}

func (s *sessionManager) revokeAll() {
	s.generation.Add(1)
}

func (s *sessionManager) csrfToken(r *http.Request, cfg config.ConsoleConfig) string {
	if !s.validate(r, cfg) {
		return ""
	}
	cookie, _ := r.Cookie("lf_session")
	mac := hmac.New(sha256.New, s.secret[:])
	mac.Write([]byte("csrf:" + cookie.Value))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *sessionManager) validCSRF(r *http.Request, cfg config.ConsoleConfig) bool {
	expected := s.csrfToken(r, cfg)
	provided := r.Header.Get("X-CSRF-Token")
	return expected != "" && provided != "" && hmac.Equal([]byte(expected), []byte(provided))
}
