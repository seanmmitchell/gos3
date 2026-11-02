package jobs

// RunNow is the admin API's durable-error-aware form of Kick. The standalone
// manager keeps its existing asynchronous wake behavior; cluster managers
// can commit the queue transition before reporting success.
func (m *Manager) RunNow(id string) error {
	if m.isClosed() {
		return ErrClosed
	}
	m.Kick(id)
	return nil
}
