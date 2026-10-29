//go:build !windows && !fakeaudio

package main

import "testing"

func TestParsePactl(t *testing.T) {
	out := `Source #52
	State: SUSPENDED
	Name: alsa_output.pci-0000_0c_00.4.analog-stereo.monitor
	Description: Monitor of Starship/Matisse HD Audio Controller Analog Stereo
	Driver: PipeWire
Source #53
	State: RUNNING
	Name: alsa_input.usb-Blue_Yeti-00.analog-stereo
	Description: Yeti Stereo Microphone Analog Stereo
	Properties:
		device.description = "Yeti Stereo Microphone"
`
	d := parsePactl(out, true)
	if len(d) != 1 || d[0].ID != "alsa_input.usb-Blue_Yeti-00.analog-stereo" || d[0].Name != "Yeti Stereo Microphone Analog Stereo" {
		t.Fatalf("%+v", d)
	}
	if len(parsePactl(out, false)) != 2 {
		t.Fatal("monitor should be listed when not skipping")
	}
}
