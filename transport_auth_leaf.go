package connect

// authSnapshot returns a copy of the current auth credential (ported from
// upstream transport.go at 20b29f79 for the credential-refusal machinery;
// the fork's auth is an atomic pointer, so this adapts the deref with a nil
// guard).

func (self *PlatformTransport) authSnapshot() ClientAuth {
	if auth := self.getAuth(); auth != nil {
		return *auth
	}
	return ClientAuth{}
}
