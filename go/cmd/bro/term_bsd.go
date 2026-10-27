//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package main

import "syscall"

// Константы ioctl termios: BSD-системы используют TIOCGETA/TIOCSETA.
const (
	ioctlReadTermios  = syscall.TIOCGETA
	ioctlWriteTermios = syscall.TIOCSETA
)
