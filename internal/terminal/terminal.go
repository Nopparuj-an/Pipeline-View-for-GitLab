package terminal

import (
	"os"
	"strconv"
	"strings"
	"time"
)

func IsTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Mouse events use SGR coordinates, which are one-based like terminal rows and columns.
type Mouse struct {
	Button, X, Y int
	Release      bool
}

func ParseMouse(s string) (Mouse, bool) {
	if !strings.HasPrefix(s, "\x1b[<") || len(s) < 7 {
		return Mouse{}, false
	}
	end := s[len(s)-1]
	if end != 'M' && end != 'm' {
		return Mouse{}, false
	}
	parts := strings.Split(s[3:len(s)-1], ";")
	if len(parts) != 3 {
		return Mouse{}, false
	}
	n := [3]int{}
	for i, part := range parts {
		v, err := strconv.Atoi(part)
		if err != nil || v < 0 || v > 100000 {
			return Mouse{}, false
		}
		n[i] = v
	}
	if n[1] == 0 || n[2] == 0 {
		return Mouse{}, false
	}
	return Mouse{Button: n[0], X: n[1], Y: n[2], Release: end == 'm'}, true
}

// keyParser buffers incomplete escape sequences across reads.
type keyParser struct{ pending []byte }

func (p *keyParser) feed(b []byte, flush bool) []string {
	p.pending = append(p.pending, b...)
	var out []string
	for len(p.pending) > 0 {
		if p.pending[0] != 0x1b {
			out = append(out, string(p.pending[0]))
			p.pending = p.pending[1:]
			continue
		}
		if len(p.pending) == 1 {
			if flush {
				out = append(out, "\x1b")
				p.pending = nil
			}
			break
		}
		if p.pending[1] != '[' && p.pending[1] != 'O' {
			out = append(out, "\x1b")
			p.pending = p.pending[1:]
			continue
		}
		j := 2
		for j < len(p.pending) && (p.pending[j] < 0x40 || p.pending[j] > 0x7e) {
			j++
		}
		if j == len(p.pending) {
			if flush || len(p.pending) > 128 {
				p.pending = nil // discard an incomplete sequence, not accidental keystrokes
			}
			break
		}
		seq := string(p.pending[:j+1])
		switch string(p.pending[2 : j+1]) {
		case "A":
			out = append(out, "up")
		case "B":
			out = append(out, "down")
		case "H", "1~", "7~":
			out = append(out, "home")
		case "F", "4~", "8~":
			out = append(out, "end")
		case "5~":
			out = append(out, "pgup")
		case "6~":
			out = append(out, "pgdn")
		default:
			if _, ok := ParseMouse(seq); ok {
				out = append(out, seq)
			}
		}
		p.pending = p.pending[j+1:]
	}
	return out
}

func ReadKeys(ch chan<- string) {
	chunks := make(chan []byte)
	go func() {
		defer close(chunks)
		buf := make([]byte, 64)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				chunks <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()
	var p keyParser
	var timeout <-chan time.Time
	for {
		select {
		case b, ok := <-chunks:
			if !ok {
				for _, k := range p.feed(nil, true) {
					ch <- k
				}
				return
			}
			for _, k := range p.feed(b, false) {
				ch <- k
			}
			if len(p.pending) > 0 {
				timeout = time.After(50 * time.Millisecond)
			} else {
				timeout = nil
			}
		case <-timeout:
			for _, k := range p.feed(nil, true) {
				ch <- k
			}
			timeout = nil
		}
	}
}
