//go:build linux || solaris

package main

import "syscall"

// Константы ioctl termios: Linux и Solaris используют TCGETS/TCSETS.
const (
	ioctlReadTermios  = syscall.TCGETS
	ioctlWriteTermios = syscall.TCSETS
)
