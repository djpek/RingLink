//go:build !windows && !fakeaudio

package main

// Linux audio through the desktop's sound server command-line tools, so no
// C libraries are needed: PipeWire/PulseAudio (parec/pacat, pw-record/pw-play)
// or plain ALSA (arecord/aplay).

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type procAudio struct {
	cmd  *exec.Cmd
	r    io.ReadCloser
	w    io.WriteCloser
	rf   *os.File // our end of the pipe (for backlog checks / pipe sizing)
	buf  []byte
	done chan struct{}

	// playback pacing: never let more than playAhead of audio queue up in the pipe and
	// the player, otherwise the pipe (64 KB = 2 s by default) fills and every voice lags behind
	start   time.Time
	written int64 // samples written since start
}

const playAhead = 80 * time.Millisecond

func (p *procAudio) Read(frame []int16) error {
	if len(p.buf) != len(frame)*2 {
		p.buf = make([]byte, len(frame)*2)
	}
	// drop mic audio that piled up (e.g. after a stall) so we always send the newest sound
	if p.rf != nil {
		if n := pipeBacklog(p.rf); n > 3*len(p.buf) {
			drop := (n - len(p.buf)) / len(p.buf) * len(p.buf)
			io.CopyN(io.Discard, p.r, int64(drop))
		}
	}
	if _, err := io.ReadFull(p.r, p.buf); err != nil {
		return err
	}
	for i := range frame {
		frame[i] = int16(binary.LittleEndian.Uint16(p.buf[2*i:]))
	}
	return nil
}

func (p *procAudio) Write(frame []int16) error {
	if len(p.buf) != len(frame)*2 {
		p.buf = make([]byte, len(frame)*2)
	}
	for i, s := range frame {
		binary.LittleEndian.PutUint16(p.buf[2*i:], uint16(s))
	}
	now := time.Now()
	if p.start.IsZero() {
		p.start = now
	}
	ahead := time.Duration(p.written)*time.Second/voiceRate - now.Sub(p.start)
	if ahead < -200*time.Millisecond { // we fell behind (stall): restart the clock instead of bursting
		p.start, p.written, ahead = now, 0, 0
	}
	if ahead > playAhead {
		time.Sleep(ahead - playAhead)
	}
	p.written += int64(len(frame))
	_, err := p.w.Write(p.buf)
	return err
}

func (p *procAudio) Close() {
	if p.r != nil {
		p.r.Close()
	}
	if p.w != nil {
		p.w.Close()
	}
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
}

var rateS = strconv.Itoa(voiceRate)

var micCmds = [][]string{
	{"parec", "--raw", "--format=s16le", "--rate=" + rateS, "--channels=1", "--latency-msec=20", "--client-name=RingLink", "--stream-name=Voice chat mic"},
	{"pw-record", "--rate", rateS, "--channels", "1", "--format", "s16", "--latency", "20ms", "-"},
	{"arecord", "-q", "-t", "raw", "-f", "S16_LE", "-r", rateS, "-c", "1", "--buffer-time=40000"},
}

var spkCmds = [][]string{
	{"pacat", "--playback", "--raw", "--format=s16le", "--rate=" + rateS, "--channels=1", "--latency-msec=60", "--client-name=RingLink", "--stream-name=Voice chat"},
	{"pw-play", "--rate", rateS, "--channels", "1", "--format", "s16", "--latency", "60ms", "-"},
	{"aplay", "-q", "-t", "raw", "-f", "S16_LE", "-r", rateS, "-c", "1", "--buffer-time=80000"},
}

func startProc(cands [][]string, capture bool, device string) (*procAudio, string, error) {
	var lastErr error = errors.New("no audio tool found (install pulseaudio-utils, pipewire or alsa-utils)")
	for _, c := range cands {
		path, err := exec.LookPath(c[0])
		if err != nil {
			continue
		}
		args := append([]string(nil), c[1:]...)
		if device != "" {
			switch c[0] {
			case "parec", "pacat":
				args = append(args, "--device="+device)
			case "pw-record", "pw-play":
				args = append([]string{"--target", device}, args...)
			}
		}
		cmd := exec.Command(path, args...)
		p := &procAudio{cmd: cmd}
		pr, pw, err := os.Pipe()
		if err != nil {
			lastErr = err
			continue
		}
		shrinkPipe(pw) // small pipe = little audio can hide in it
		var other *os.File
		if capture {
			cmd.Stdout, p.r, p.rf, other = pw, pr, pr, pw
		} else {
			cmd.Stdin, p.w, other = pr, pw, pr
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			pr.Close()
			pw.Close()
			lastErr = err
			continue
		}
		other.Close() // the child has its own copy now
		// make sure the tool actually works (it exits at once when there's no sound server)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		p.done = done
		select {
		case <-done:
			lastErr = fmt.Errorf("%s: %s", c[0], strings.TrimSpace(stderr.String()))
			continue
		case <-time.After(400 * time.Millisecond):
		}
		return p, c[0], nil
	}
	return nil, "", lastErr
}

func openMic(device string) (micSource, string, error) {
	p, name, err := startProc(micCmds, true, device)
	if err != nil {
		return nil, "", err
	}
	return p, name, nil
}

func openSpeaker(device string) (speakerSink, string, error) {
	p, name, err := startProc(spkCmds, false, device)
	if err != nil {
		return nil, "", err
	}
	return p, name, nil
}

// Device lists come from the sound server (PipeWire or PulseAudio) via pactl.
// IDs are sound-server device names, which parec/pacat and pw-record/pw-play
// both accept.

func listMics() []AudioDevice     { return pactlList("sources", true) }
func listSpeakers() []AudioDevice { return pactlList("sinks", false) }

func pactlList(kind string, skipMonitors bool) []AudioDevice {
	path, err := exec.LookPath("pactl")
	if err != nil {
		return nil
	}
	cmd := exec.Command(path, "list", kind)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parsePactl(string(out), skipMonitors)
}

func parsePactl(out string, skipMonitors bool) []AudioDevice {
	var devs []AudioDevice
	var cur AudioDevice
	flush := func() {
		if cur.ID != "" && !(skipMonitors && strings.HasSuffix(cur.ID, ".monitor")) {
			if cur.Name == "" {
				cur.Name = cur.ID
			}
			devs = append(devs, cur)
		}
		cur = AudioDevice{}
	}
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Source #") || strings.HasPrefix(line, "Sink #"):
			flush()
		case strings.HasPrefix(t, "Name: ") && cur.ID == "":
			cur.ID = strings.TrimPrefix(t, "Name: ")
		case strings.HasPrefix(t, "Description: ") && cur.Name == "":
			cur.Name = strings.TrimPrefix(t, "Description: ")
		}
	}
	flush()
	return devs
}
