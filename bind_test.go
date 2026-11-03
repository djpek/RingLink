package main

import "testing"

func TestBinderCaptureAndHold(t *testing.T) {
	b := NewBinder()
	var held []bool
	var got string
	b.onChange = func(h bool) { held = append(held, h) }
	b.onCapture = func(id string) { got = id }
	b.Capture()
	b.step(map[string]bool{"key:13": true})                  // already held when capture began: ignored
	b.step(map[string]bool{"key:13": true, "joy0:b5": true}) // new press -> becomes the binding
	if got != "joy0:b5" || b.Bind() != "joy0:b5" || b.Capturing() {
		t.Fatalf("captured %q bind %q capturing %v", got, b.Bind(), b.Capturing())
	}
	if len(held) != 0 {
		t.Fatalf("the press that sets the binding must not broadcast: %v", held)
	}
	b.step(map[string]bool{})
	b.step(map[string]bool{"joy0:b5": true})
	b.step(map[string]bool{"joy0:b5": true})
	b.step(map[string]bool{"key:66": true})
	if len(held) != 2 || !held[0] || held[1] {
		t.Fatalf("held changes %v", held)
	}
	if bindName("joy0:b5") != "Controller 1 button 6" || bindName("key:66") != "Key B" || bindName("key:112") != "F1" {
		t.Fatal(bindName("joy0:b5"), bindName("key:66"), bindName("key:112"))
	}
}
