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
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type procAudio struct {
	cmd  *exec.Cmd
	r    io.ReadCloser
	w    io.WriteCloser
	buf  []byte
	done chan struct{}
}

func (p *procAudio) Read(frame []int16) error {
	if len(p.buf) != len(frame)*2 {
		p.buf = make([]byte, len(frame)*2)
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

func startProc(cands [][]string, capture bool) (*procAudio, string, error) {
	var lastErr error = errors.New("no audio tool found (install pulseaudio-utils, pipewire or alsa-utils)")
	for _, c := range cands {
		path, err := exec.LookPath(c[0])
		if err != nil {
			continue
		}
		cmd := exec.Command(path, c[1:]...)
		p := &procAudio{cmd: cmd}
		if capture {
			p.r, err = cmd.StdoutPipe()
		} else {
			p.w, err = cmd.StdinPipe()
		}
		if err != nil {
			lastErr = err
			continue
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			lastErr = err
			continue
		}
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

func openMic() (micSource, string, error) {
	p, name, err := startProc(micCmds, true)
	if err != nil {
		return nil, "", err
	}
	return p, name, nil
}

func openSpeaker() (speakerSink, string, error) {
	p, name, err := startProc(spkCmds, false)
	if err != nil {
		return nil, "", err
	}
	return p, name, nil
}
