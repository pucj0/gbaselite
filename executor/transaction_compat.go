package executor

// SetMVCCAutocommit preserves the public Go API. Deprecated: use SetAutocommit.
func (e *Engine) SetMVCCAutocommit(session *Session, enabled bool) error {
	return e.SetAutocommit(session, enabled)
}
