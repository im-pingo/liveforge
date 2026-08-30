package httpstream

import (
	"net/http"
	"time"

	"github.com/im-pingo/liveforge/core"
)

// serveDASHManifest serves the MPD manifest for a stream.
func (m *Module) serveDASHManifest(w http.ResponseWriter, r *http.Request, streamKey string) {
	stream, found := m.server.StreamHub().Find(streamKey)
	if !found || stream.State() != core.StreamStatePublishing {
		http.Error(w, "stream not found or not publishing", http.StatusNotFound)
		return
	}

	mgr := m.getOrCreateDASH(streamKey, stream)
	if mgr == nil {
		http.Error(w, "stream generation ended", http.StatusNotFound)
		return
	}

	// One completed keyframe-bounded segment is enough because the MPD uses an
	// explicit SegmentTimeline. Waiting for a fixed three-segment buffer makes
	// startup roughly three GOPs long for publishers with sparse keyframes.
	if !waitForCondition(r.Context(), 15*time.Second, 100*time.Millisecond, func() bool {
		return mgr.SegmentCount() >= 1 || mgr.isTerminal()
	}) {
		if r.Context().Err() != nil {
			return
		}
		writeHTTPError(w, "initial DASH segment not ready", http.StatusServiceUnavailable)
		return
	}
	if mgr.SegmentCount() < 1 {
		writeHTTPError(w, "initial DASH segment not ready", http.StatusServiceUnavailable)
		return
	}

	manifest := mgr.GenerateMPD()

	m.setCORSHeaders(w)
	w.Header().Set("Content-Type", "application/dash+xml")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	_ = writeHTTPResponse(w, []byte(manifest), httpStreamWriteTimeout)
}

// serveDASHInit serves the fMP4 init segment.
func (m *Module) serveDASHInit(w http.ResponseWriter, r *http.Request, streamKey string) {
	m.dashMu.Lock()
	mgr, ok := m.dashManagers[streamKey]
	m.dashMu.Unlock()

	if !ok {
		http.Error(w, "no DASH session for this stream", http.StatusNotFound)
		return
	}

	data, found := mgr.GetInitSegment()
	if !found {
		http.Error(w, "init segment not ready", http.StatusNotFound)
		return
	}

	m.setCORSHeaders(w)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	_ = writeHTTPResponse(w, data, httpStreamWriteTimeout)
}

// serveDASHSegment serves a single fMP4 media segment by sequence number.
//
// Production hold: if the segment hasn't been produced yet (future segment),
// wait up to ~6s for it to appear. ffmpeg's dashdec.c has no backoff on 404.
//
// No availability hold is needed because the MPD uses SegmentTimeline with
// explicit per-segment timing, so ffmpeg advances segment numbers from the
// timeline rather than computing them from wall-clock time.
func (m *Module) serveDASHSegment(w http.ResponseWriter, r *http.Request, streamKey string, seqNum int) {
	m.dashMu.Lock()
	mgr, ok := m.dashManagers[streamKey]
	m.dashMu.Unlock()

	if !ok {
		http.Error(w, "no DASH session for this stream", http.StatusNotFound)
		return
	}

	// Production hold — wait for the segment to be produced.
	data, found := mgr.GetSegment(seqNum)
	if !found {
		_, hi := mgr.SegmentRange()
		if seqNum > hi {
			if !waitForCondition(r.Context(), 6*time.Second, 100*time.Millisecond, func() bool {
				data, found = mgr.GetSegment(seqNum)
				return found || mgr.isTerminal()
			}) && r.Context().Err() != nil {
				return
			}
		}
	}

	if !found {
		writeHTTPError(w, "segment not found", http.StatusNotFound)
		return
	}

	m.setCORSHeaders(w)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "public, max-age=10")
	_ = writeHTTPResponse(w, data, httpStreamWriteTimeout)
}

// serveDASHAudioInit serves the audio-only fMP4 init segment.
func (m *Module) serveDASHAudioInit(w http.ResponseWriter, r *http.Request, streamKey string) {
	m.dashMu.Lock()
	mgr, ok := m.dashManagers[streamKey]
	m.dashMu.Unlock()

	if !ok {
		http.Error(w, "no DASH session for this stream", http.StatusNotFound)
		return
	}

	data, found := mgr.GetAudioInitSegment()
	if !found {
		http.Error(w, "audio init segment not ready", http.StatusNotFound)
		return
	}

	m.setCORSHeaders(w)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	_ = writeHTTPResponse(w, data, httpStreamWriteTimeout)
}

// serveDASHAudioSegment serves a single audio-only fMP4 segment by sequence number.
func (m *Module) serveDASHAudioSegment(w http.ResponseWriter, r *http.Request, streamKey string, seqNum int) {
	m.dashMu.Lock()
	mgr, ok := m.dashManagers[streamKey]
	m.dashMu.Unlock()

	if !ok {
		http.Error(w, "no DASH session for this stream", http.StatusNotFound)
		return
	}

	// Production hold — wait for the segment to be produced.
	data, found := mgr.GetAudioSegment(seqNum)
	if !found {
		_, hi := mgr.SegmentRange()
		if seqNum > hi {
			if !waitForCondition(r.Context(), 6*time.Second, 100*time.Millisecond, func() bool {
				data, found = mgr.GetAudioSegment(seqNum)
				return found || mgr.isTerminal()
			}) && r.Context().Err() != nil {
				return
			}
		}
	}

	if !found {
		writeHTTPError(w, "audio segment not found", http.StatusNotFound)
		return
	}

	m.setCORSHeaders(w)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "public, max-age=10")
	_ = writeHTTPResponse(w, data, httpStreamWriteTimeout)
}
