package main

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"time"
)

// Minimal STUN (RFC 5389) binding request, used to learn our public address.

var stunServers = []string{
	"stun.l.google.com:19302",
	"stun1.l.google.com:19302",
	"stun.cloudflare.com:3478",
}

const stunMagic = 0x2112A442

func isSTUN(b []byte) bool {
	return len(b) >= 20 && b[0]&0xC0 == 0 && binary.BigEndian.Uint32(b[4:8]) == stunMagic
}

func stunRequest() (req []byte, txid [12]byte) {
	rand.Read(txid[:])
	req = make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:], 0x0001) // binding request
	binary.BigEndian.PutUint16(req[2:], 0)
	binary.BigEndian.PutUint32(req[4:], stunMagic)
	copy(req[8:], txid[:])
	return
}

func parseSTUN(b []byte, txid [12]byte) (*net.UDPAddr, error) {
	if !isSTUN(b) || binary.BigEndian.Uint16(b[0:]) != 0x0101 {
		return nil, errors.New("not a binding success")
	}
	if string(b[8:20]) != string(txid[:]) {
		return nil, errors.New("txid mismatch")
	}
	n := int(binary.BigEndian.Uint16(b[2:]))
	attrs := b[20:]
	if len(attrs) < n {
		return nil, errors.New("short")
	}
	attrs = attrs[:n]
	var mapped *net.UDPAddr
	for len(attrs) >= 4 {
		t := binary.BigEndian.Uint16(attrs[0:])
		l := int(binary.BigEndian.Uint16(attrs[2:]))
		if len(attrs) < 4+l {
			break
		}
		v := attrs[4 : 4+l]
		if (t == 0x0020 || t == 0x0001) && l >= 8 && v[1] == 0x01 {
			port := binary.BigEndian.Uint16(v[2:])
			ip := net.IPv4(v[4], v[5], v[6], v[7]).To4()
			if t == 0x0020 {
				port ^= uint16(stunMagic >> 16)
				var c [4]byte
				binary.BigEndian.PutUint32(c[:], stunMagic)
				for i := 0; i < 4; i++ {
					ip[i] ^= c[i]
				}
				return &net.UDPAddr{IP: ip, Port: int(port)}, nil
			}
			mapped = &net.UDPAddr{IP: ip, Port: int(port)}
		}
		attrs = attrs[4+((l+3)&^3):]
	}
	if mapped != nil {
		return mapped, nil
	}
	return nil, errors.New("no mapped address")
}

// discover sends STUN requests through send(). Responses are routed back by
// the caller's read loop through ch.
func stunDiscover(send func([]byte, *net.UDPAddr), ch chan []byte) (pub *net.UDPAddr, symmetric bool, err error) {
	var results []*net.UDPAddr
	for _, srv := range stunServers {
		ra, e := net.ResolveUDPAddr("udp4", srv)
		if e != nil {
			continue
		}
		req, txid := stunRequest()
		for attempt := 0; attempt < 3; attempt++ {
			send(req, ra)
			deadline := time.After(700 * time.Millisecond)
			got := false
		wait:
			for {
				select {
				case b := <-ch:
					if a, e := parseSTUN(b, txid); e == nil {
						results = append(results, a)
						got = true
						break wait
					}
				case <-deadline:
					break wait
				}
			}
			if got {
				break
			}
		}
		if len(results) >= 2 {
			break
		}
	}
	if len(results) == 0 {
		return nil, false, errors.New("could not reach any STUN server (no internet, or UDP blocked)")
	}
	if len(results) >= 2 && results[0].Port != results[1].Port {
		symmetric = true
	}
	return results[0], symmetric, nil
}
