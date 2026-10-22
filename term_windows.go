package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

const (
	enableProcessedInput            = 0x0001
	enableLineInput                 = 0x0002
	enableEchoInput                 = 0x0004
	enableVirtualTerminalInput      = 0x0200
	enableVirtualTerminalProcessing = 0x0004
)

func termSize() (int, int) {
	var info struct {
		size, cursor             [2]int16
		attrs                    uint16
		left, top, right, bottom int16
		maxSize                  [2]int16
	}
	r, _, _ := procGetConsoleScreenBufferInfo.Call(os.Stdout.Fd(), uintptr(unsafe.Pointer(&info)))
	w, h := int(info.right-info.left)+1, int(info.bottom-info.top)+1
	if r == 0 || w <= 1 || h <= 1 {
		return 100, 30
	}
	return w, h
}

func setConsoleMode(h syscall.Handle, mode uint32) error {
	if r, _, err := procSetConsoleMode.Call(uintptr(h), uintptr(mode)); r == 0 {
		return err
	}
	return nil
}

// rawMode switches the console to unbuffered, no-echo input with VT sequences on both
// sides (so arrow keys arrive as ESC [ A and our ANSI output renders) and returns a restore func.
func rawMode() (func(), error) {
	in, out := syscall.Handle(os.Stdin.Fd()), syscall.Handle(os.Stdout.Fd())
	var inMode, outMode uint32
	if err := syscall.GetConsoleMode(in, &inMode); err != nil {
		return nil, fmt.Errorf("console: %w", err)
	}
	if err := syscall.GetConsoleMode(out, &outMode); err != nil {
		return nil, fmt.Errorf("console: %w", err)
	}
	if err := setConsoleMode(in, inMode&^(enableLineInput|enableEchoInput|enableProcessedInput)|enableVirtualTerminalInput); err != nil {
		return nil, fmt.Errorf("console: %w", err)
	}
	if err := setConsoleMode(out, outMode|enableVirtualTerminalProcessing); err != nil {
		setConsoleMode(in, inMode)
		return nil, fmt.Errorf("console: %w", err)
	}
	return func() { setConsoleMode(in, inMode); setConsoleMode(out, outMode) }, nil
}
