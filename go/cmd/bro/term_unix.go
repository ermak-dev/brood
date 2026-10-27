//go:build unix

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// readLineRaw читает строку байтов до '\n' без подсказки и без буфера.
func readLineRaw(f *os.File) (string, error) {
	return readLine(f, "", nil)
}

// isTerminal проверяет, является ли файл терминалом (ioctl чтения режима).
func isTerminal(f *os.File) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(ioctlReadTermios),
		uintptr(unsafe.Pointer(&termios)),
		0, 0, 0,
	)
	return errno == 0
}

// readPasswordTerminal выключает эхо на время чтения пароля и возвращает
// управление терминалу при любом исходе. Эхо-выключение — единственное
// изменение режима: канонический ввод сам даёт построчное чтение.
func readPasswordTerminal(f *os.File, _ *os.File) (string, error) {
	var saved syscall.Termios
	if _, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(ioctlReadTermios),
		uintptr(unsafe.Pointer(&saved)),
		0, 0, 0,
	); errno != 0 {
		// Терминал недоступен: читаем как есть; вызывающий уже напечатал
		// приглашение.
		return readLineRaw(f)
	}
	noEcho := saved
	noEcho.Lflag &^= syscall.ECHO
	if _, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(ioctlWriteTermios),
		uintptr(unsafe.Pointer(&noEcho)),
		0, 0, 0,
	); errno != 0 {
		return readLineRaw(f)
	}
	line, err := readLineRaw(f)
	// Восстанавливаем эхо обязательно, даже при ошибке чтения.
	syscall.Syscall6(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(ioctlWriteTermios),
		uintptr(unsafe.Pointer(&saved)),
		0, 0, 0,
	)
	return line, err
}
