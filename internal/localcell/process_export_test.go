package localcell

// RecordAliveForTest checks the same native creation identity as lifecycle
// control, even after Remove has erased the durable record. It grants no signal
// authority and is available only in test builds.
func RecordAliveForTest(record ProcessRecord) (bool, error) {
	identity, alive, err := processIdentity(record.PID)
	return alive && identity == record.StartTime, err
}
