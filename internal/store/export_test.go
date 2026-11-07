package store

// SetSyncPrincipalSingleLoginForTest exposes only the package-test activation
// exception to the external executable rehearsal. It is absent from every
// shipped binary and does not weaken migration or runtime role checks.
func SetSyncPrincipalSingleLoginForTest(s *Store) {
	if s.syncPrincipalTest == nil {
		s.syncPrincipalTest = &syncPrincipalTestOptions{}
	}
	s.syncPrincipalTest.singleLogin = true
}
