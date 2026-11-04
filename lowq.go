package main

// Low-quality voice for people far away in big rooms: still understandable, about a
// fifth of the upload. 8 kHz IMA ADPCM (4 bits a sample) in 60 ms packets instead of
// 24 kHz mu-law in 20 ms packets (per-packet overhead is ~80 bytes, so fewer packets
// matter as much as fewer bits). Each packet carries its own decoder state, so a lost
// packet doesn't garble the next one.
//
// packet: [vkLow][seq u16][predictor i16][index u8][240 bytes ADPCM = 480 samples @ 8 kHz]

import (
	"encoding/binary"
	"math"
)

const (
	vkLow       = 5
	lowRate     = 8000
	lowFrames   = 3                                            // 24 kHz frames per low packet (60 ms)
	lowSamples  = voiceFrame * lowFrames * lowRate / voiceRate // 480
	lowPktBytes = 1 + 2 + 2 + 1 + lowSamples/2
)

var imaStep = [89]int32{7, 8, 9, 10, 11, 12, 13, 14, 16, 17, 19, 21, 23, 25, 28, 31, 34, 37, 41, 45,
	50, 55, 60, 66, 73, 80, 88, 97, 107, 118, 130, 143, 157, 173, 190, 209, 230, 253, 279, 307, 337, 371,
	408, 449, 494, 544, 598, 658, 724, 796, 876, 963, 1060, 1166, 1282, 1411, 1552, 1707, 1878, 2066,
	2272, 2499, 2749, 3024, 3327, 3660, 4026, 4428, 4871, 5358, 5894, 6484, 7132, 7845, 8630, 9493,
	10442, 11487, 12635, 13899, 15289, 16818, 18500, 20350, 22385, 24623, 27086, 29794, 32767}
var imaIdx = [16]int32{-1, -1, -1, -1, 2, 4, 6, 8, -1, -1, -1, -1, 2, 4, 6, 8}

type adpcm struct {
	pred  int32
	index int32
}

func (s *adpcm) decodeNibble(n byte) int16 {
	step := imaStep[s.index]
	d := step >> 3
	if n&1 != 0 {
		d += step >> 2
	}
	if n&2 != 0 {
		d += step >> 1
	}
	if n&4 != 0 {
		d += step
	}
	if n&8 != 0 {
		s.pred -= d
	} else {
		s.pred += d
	}
	s.pred = max(-32768, min(32767, s.pred))
	s.index = max(0, min(88, s.index+imaIdx[n]))
	return int16(s.pred)
}

func (s *adpcm) encodeSample(x int16) byte {
	step := imaStep[s.index]
	diff := int32(x) - s.pred
	var n byte
	if diff < 0 {
		n, diff = 8, -diff
	}
	if diff >= step {
		n |= 4
		diff -= step
	}
	if diff >= step>>1 {
		n |= 2
		diff -= step >> 1
	}
	if diff >= step>>2 {
		n |= 1
	}
	s.decodeNibble(n) // keep the encoder's state exactly where the decoder's will be
	return n
}

// lowEncoder collects 3 mic frames and turns them into one low-quality packet.
type lowEncoder struct {
	buf []int16 // 24 kHz samples waiting
	st  adpcm
	seq uint16
	lp  [2]float64 // anti-alias low-pass state
}

// add one 24 kHz frame; returns a packet every third frame.
func (e *lowEncoder) add(fr []int16) []byte {
	e.buf = append(e.buf, fr...)
	if len(e.buf) < voiceFrame*lowFrames {
		return nil
	}
	return e.flush()
}

// flush sends whatever is waiting (padded with silence), e.g. when you stop talking.
func (e *lowEncoder) flush() []byte {
	if len(e.buf) == 0 {
		return nil
	}
	for len(e.buf) < voiceFrame*lowFrames {
		e.buf = append(e.buf, 0)
	}
	pkt := make([]byte, lowPktBytes)
	pkt[0] = vkLow
	binary.BigEndian.PutUint16(pkt[1:], e.seq)
	e.seq++
	binary.BigEndian.PutUint16(pkt[3:], uint16(int16(e.st.pred)))
	pkt[5] = byte(e.st.index)
	// low-pass at ~3.4 kHz (two one-pole stages) then keep every third sample
	a := 1 - math.Exp(-2*math.Pi*3400/voiceRate)
	for i := 0; i < lowSamples; i++ {
		var y float64
		for k := 0; k < 3; k++ {
			x := float64(e.buf[3*i+k])
			e.lp[0] += a * (x - e.lp[0])
			e.lp[1] += a * (e.lp[0] - e.lp[1])
			y = e.lp[1]
		}
		n := e.st.encodeSample(int16(max(-32768, min(32767, y))))
		if i%2 == 0 {
			pkt[6+i/2] = n
		} else {
			pkt[6+i/2] |= n << 4
		}
	}
	e.buf = e.buf[:0]
	return pkt
}

// decodeLow turns a low-quality packet back into three 24 kHz frames.
func decodeLow(b []byte) [][]int16 {
	if len(b) != lowPktBytes || b[0] != vkLow {
		return nil
	}
	st := adpcm{pred: int32(int16(binary.BigEndian.Uint16(b[3:]))), index: int32(min(88, b[5]))}
	low := make([]float64, lowSamples+1)
	for i := 0; i < lowSamples; i++ {
		n := b[6+i/2]
		if i%2 == 1 {
			n >>= 4
		}
		low[i] = float64(st.decodeNibble(n & 15))
	}
	low[lowSamples] = low[lowSamples-1]
	out := make([][]int16, lowFrames)
	for f := range out {
		fr := make([]int16, voiceFrame)
		for i := range fr {
			j := f*voiceFrame + i // 24 kHz sample index
			t := float64(j) / 3
			k := int(t)
			fr[i] = int16(low[k] + (low[k+1]-low[k])*(t-float64(k)))
		}
		out[f] = fr
	}
	return out
}
