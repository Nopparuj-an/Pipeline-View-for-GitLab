package terminal

import (
	"os"
)

func IsTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func ReadKeys(ch chan<- string) {
	buf := make([]byte, 64)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return
		}
		for _, k := range parseKeys(buf[:n]) {
			ch <- k
		}
	}
}

func parseKeys(b []byte) []string {
	var out []string
	for i := 0; i < len(b); {
		if b[i] == 0x1b && i+2 < len(b) && (b[i+1] == '[' || b[i+1] == 'O') {
			j := i + 2
			for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
				j++
			}
			if j < len(b) {
				switch string(b[i+2 : j+1]) {
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
				}
				i = j + 1
				continue
			}
		}
		out = append(out, string(b[i]))
		i++
	}
	return out
}
