package auth

// Snapshot copies identity atomically without copying its mutex. The snapshot
// must not be published back to the pool. Chat snapshots never expose refresh tokens.
func (a *Auth) Snapshot(refresh bool) *Auth {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s := &Auth{AccessToken: a.AccessToken, ExpiresAt: a.ExpiresAt, Domain: a.Domain, realm: a.realmLocked(), UID: a.UID, EnterpriseID: a.EnterpriseID, Nickname: a.Nickname, DeviceToken: a.DeviceToken, expiresAtMs: a.expiresAtMs}
	if refresh {
		s.RefreshToken = a.RefreshToken
	}
	if a.useProxy != nil {
		v := *a.useProxy
		s.useProxy = &v
	}
	return s
}
