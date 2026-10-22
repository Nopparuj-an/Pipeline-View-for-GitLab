//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

func termSize() (int, int) {
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.Col == 0 || ws.Row == 0 {
		return 100, 30
	}
	return int(ws.Col), int(ws.Row)
}

func stty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// rawMode switches the terminal to unbuffered, no-echo input and returns a restore func.
func rawMode() (func(), error) {
	saved, err := stty("-g")
	if err != nil {
		return nil, fmt.Errorf("stty: %w", err)
	}
	if _, err := stty("-icanon", "-echo", "min", "1", "time", "0"); err != nil {
		return nil, fmt.Errorf("stty: %w", err)
	}
	return func() { stty(saved) }, nil
}
