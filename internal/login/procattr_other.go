//go:build !linux

package login

import "syscall"

func sysProcAttr() *syscall.SysProcAttr { return nil }
