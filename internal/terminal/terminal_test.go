package terminal

import (
	"reflect"
	"testing"
)

func TestKeyParserSplitSequences(t *testing.T) {
	var p keyParser
	var got []string
	for _, part := range []string{"j\x1b", "[<", "0;12", ";", "34", "M\x1b[", "Aq"} {
		got = append(got, p.feed([]byte(part), false)...)
	}
	want := []string{"j", "\x1b[<0;12;34M", "up", "q"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if e, ok := ParseMouse(got[1]); !ok || e != (Mouse{X: 12, Y: 34}) {
		t.Fatalf("mouse = %+v, %v", e, ok)
	}
}

func TestKeyParserEscapeAndMalformedMouse(t *testing.T) {
	var p keyParser
	if got := p.feed([]byte("\x1b"), false); len(got) != 0 {
		t.Fatal(got)
	}
	if got := p.feed(nil, true); !reflect.DeepEqual(got, []string{"\x1b"}) {
		t.Fatal(got)
	}
	if got := p.feed([]byte("\x1b[<0;2;M\x1b[<64;5;6M"), false); !reflect.DeepEqual(got, []string{"\x1b[<64;5;6M"}) {
		t.Fatal(got)
	}
	if _, ok := ParseMouse("\x1b[<0;0;2M"); ok {
		t.Fatal("accepted zero coordinate")
	}
}

func TestKeyParserMouseMotion(t *testing.T) {
	var p keyParser
	got := p.feed([]byte("\x1b[<35;12;4M"), false)
	if len(got) != 1 {
		t.Fatal(got)
	}
	if e, ok := ParseMouse(got[0]); !ok || e != (Mouse{Button: 35, X: 12, Y: 4}) {
		t.Fatalf("mouse motion = %+v, %v", e, ok)
	}
}
