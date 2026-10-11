package executor

import "syscall"

// ProtectServerProcess stops other processes of the same OS user (agents
// under the local executor) from reading luxd's environment and memory
// through /proc or ptrace. Provider keys and the database URL live there.
// Runtimes started later are unaffected: exec makes a process dumpable again.
func ProtectServerProcess() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
