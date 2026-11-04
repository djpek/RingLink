package main

import "testing"

func TestBinderCaptureAndHold(t *testing.T) {
	b := NewBinder()
	type ev struct {
		act  string
		held bool
	}
	var evs []ev
	var gotAct, got string
	b.onChange = func(a string, h bool) { evs = append(evs, ev{a, h}) }
	b.onCapture = func(a, id string) { gotAct, got = a, id }
	b.Capture(actPTT)
	b.step(map[string]bool{"key:13": true})                  // already held when capture began: ignored
	b.step(map[string]bool{"key:13": true, "joy0:b5": true}) // new press -> becomes the binding
	if gotAct != actPTT || got != "joy0:b5" || b.Bind(actPTT) != "joy0:b5" || b.CapturingFor() != "" {
		t.Fatalf("captured %q %q bind %q", gotAct, got, b.Bind(actPTT))
	}
	b.step(map[string]bool{"joy0:b5": true}) // still held from binding: must not fire
	if len(evs) != 0 {
		t.Fatalf("the press that sets the binding must not fire: %v", evs)
	}
	b.SetBind(actPTM, "key:77")
	b.step(map[string]bool{})
	b.step(map[string]bool{"joy0:b5": true})
	b.step(map[string]bool{"joy0:b5": true, "key:77": true})
	b.step(map[string]bool{"key:66": true})
	want := []ev{{actPTT, true}, {actPTM, true}, {actPTT, false}, {actPTM, false}}
	if len(evs) != len(want) {
		t.Fatalf("events %v", evs)
	}
	for i := range want {
		if evs[i] != want[i] {
			t.Fatalf("events %v, want %v", evs, want)
		}
	}
	if bindName("joy0:b5") != "Controller 1 button 6" || bindName("key:66") != "Key B" || bindName("key:112") != "F1" {
		t.Fatal(bindName("joy0:b5"), bindName("key:66"), bindName("key:112"))
	}
}
