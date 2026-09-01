//go:build windows && amd64

package main

import (
	"testing"
	"unsafe"
)

func TestWin64InputABI(t *testing.T) {
	if got := unsafe.Sizeof(input{}); got != 40 {
		t.Fatalf("sizeof(INPUT) = %d, want 40", got)
	}
	if got := unsafe.Offsetof(input{}.Data); got != 8 {
		t.Fatalf("offsetof(INPUT.union) = %d, want 8", got)
	}
	if got := unsafe.Sizeof(mouseInput{}); got != 32 {
		t.Fatalf("sizeof(MOUSEINPUT) = %d, want 32", got)
	}
	if got := unsafe.Sizeof(keyboardInput{}); got != 24 {
		t.Fatalf("sizeof(KEYBDINPUT) = %d, want 24", got)
	}
}

func TestRectsOverlap(t *testing.T) {
	tests := []struct {
		name string
		a    rect
		b    rect
		want bool
	}{
		{name: "area overlap", a: rect{0, 0, 100, 40}, b: rect{90, 10, 150, 50}, want: true},
		{name: "touching edge is not overlap", a: rect{0, 0, 100, 40}, b: rect{100, 0, 150, 40}, want: false},
		{name: "separate", a: rect{0, 0, 50, 50}, b: rect{60, 60, 100, 100}, want: false},
		{name: "contained", a: rect{0, 0, 100, 100}, b: rect{20, 20, 30, 30}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rectsOverlap(tt.a, tt.b); got != tt.want {
				t.Fatalf("rectsOverlap(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestParseVirtualKey(t *testing.T) {
	for input, want := range map[string]uint16{
		"R":      'R',
		"r":      'R',
		"S":      'S',
		"A":      'A',
		"Esc":    vkEscape,
		"escape": vkEscape,
	} {
		got, err := parseVirtualKey(input)
		if err != nil {
			t.Fatalf("parseVirtualKey(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("parseVirtualKey(%q) = %#x, want %#x", input, got, want)
		}
	}
	if _, err := parseVirtualKey("X"); err == nil {
		t.Fatal("parseVirtualKey(X) unexpectedly succeeded")
	}
}

func TestFindButtonOverlapsIgnoresHiddenAndGroupBox(t *testing.T) {
	controls := []control{
		{ID: 1, Class: "Button", Text: "one", Rect: rect{0, 0, 100, 40}, Visible: true},
		{ID: 2, Class: "Button", Text: "two", Rect: rect{50, 0, 150, 40}, Visible: true},
		{ID: 3, Class: "Button", Text: "hidden", Rect: rect{0, 0, 100, 40}, Visible: false},
		{ID: 4, Class: "Button", Text: "group", Rect: rect{0, 0, 200, 100}, Style: bsGroupBox, Visible: true},
	}

	got := findButtonOverlaps(controls)
	if len(got) != 1 {
		t.Fatalf("findButtonOverlaps returned %d pairs, want 1: %#v", len(got), got)
	}
	if got[0][0].ID != 1 || got[0][1].ID != 2 {
		t.Fatalf("unexpected overlap pair: %#v", got[0])
	}
}
