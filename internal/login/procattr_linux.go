package login

import "syscall"

// sysProcAttr makes the login proxy die with us even if we're SIGKILLed.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
