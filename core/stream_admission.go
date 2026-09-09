package core

// DiscardFailedAdmission retires a newly created candidate only if it has no
// consumers and no publisher other than the failed attempt has ever owned it.
// Callers must have obtained created=true from GetOrCreateWithCreated.
func (s *Stream) DiscardFailedAdmission(publisherID string) bool {
	s.writeMu.Lock()
	s.mu.Lock()
	if publisherID == "" || s.state == StreamStateDestroying || s.publisher != nil || s.subscriberTotal.Load() != 0 || (s.lastPublisherID != "" && s.lastPublisherID != publisherID) {
		s.mu.Unlock()
		s.writeMu.Unlock()
		return false
	}
	if s.noPublisherTimer != nil {
		s.noPublisherTimer.Stop()
		s.noPublisherTimer = nil
	}
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.closeGenerationLocked()
	s.startupReady = false
	s.usedPublisherIDs = nil
	s.state = StreamStateDestroying
	s.signalStartupStateChangedLocked()
	s.mu.Unlock()
	s.writeMu.Unlock()
	s.Close()
	return true
}
