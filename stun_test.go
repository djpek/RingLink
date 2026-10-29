package main

import (
	"encoding/hex"
	"testing"
)

// RFC 5769 section 2.2: XOR-MAPPED-ADDRESS attribute for 192.0.2.1:32853
func TestSTUNVector(t *testing.T) {
	msg, _ := hex.DecodeString("0101000c2112a442b7e7a701bc34d686fa87dfae" + "002000080001a147e112a643")
	var tx [12]byte
	copy(tx[:], msg[8:20])
	a, err := parseSTUN(msg, tx)
	if err != nil {
		t.Fatal(err)
	}
	if a.String() != "192.0.2.1:32853" {
		t.Fatalf("got %s", a)
	}
}
