package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"testing"
)

// obfuscated2Init builds a client header the way Telegram clients do:
// random bytes, the transport tag at 56–60, then bytes 56–64 replaced by
// their encryption under the key/IV the header itself carries.
func obfuscated2Init(t *testing.T, tag uint32) []byte {
	t.Helper()
	b := make([]byte, mtprotoInitLen)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(b[56:60], tag)
	block, err := aes.NewCipher(b[8:40])
	if err != nil {
		t.Fatal(err)
	}
	enc := make([]byte, mtprotoInitLen)
	cipher.NewCTR(block, b[40:56]).XORKeyStream(enc, b)
	copy(b[56:], enc[56:])
	return b
}

func TestLooksLikeMTProto(t *testing.T) {
	for _, tag := range []uint32{mtprotoTagAbridged, mtprotoTagIntermediate, mtprotoTagPadded} {
		b := append(obfuscated2Init(t, tag), 0x01, 0x02, 0x03)
		if !looksLikeMTProto(b) {
			t.Errorf("tag %#x not recognised", tag)
		}
	}
	random := make([]byte, 128)
	_, _ = rand.Read(random)
	if looksLikeMTProto(random) {
		t.Error("random bytes recognised as MTProto (1 in 2^30 — rerun if this ever flakes)")
	}
	if looksLikeMTProto(obfuscated2Init(t, mtprotoTagAbridged)[:63]) {
		t.Error("short header recognised")
	}
	hello := []byte{0x16, 0x03, 0x01, 0x02, 0x00}
	if looksLikeMTProto(append(hello, make([]byte, 64)...)) {
		t.Error("TLS ClientHello recognised as MTProto")
	}
}
