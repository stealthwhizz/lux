//go:build !linux

package executor

// ProtectServerProcess is a no-op outside Linux; the local executor is only
// supported there.
func ProtectServerProcess() error { return nil }
