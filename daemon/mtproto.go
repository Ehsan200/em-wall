package main

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
)

// Recognising Telegram's MTProto transport.
//
// Telegram clients reach their data centres by IP on 443/80/5222 with the
// obfuscated2 transport, not TLS, so looksLikeTLSClientHello never matched
// and every Telegram connection was forwarded unverified: the first-ranked
// member was used blindly, and when its exit was dead (xray answers SOCKS
// CONNECT before dialing) the connection sat silent until the client gave
// up — no failover, and the DC's IP then paused by tcpHealth. Observed as
// Telegram stuck on "connecting" while the set had working members.
//
// MTProto is as verifiable as TLS: the client's first packet (req_pq) is
// answered within an RTT, and it carries no side effects, so it can be
// replayed on a sibling. Detection is exact for direct connections: the
// 64-byte obfuscated2 header carries its own AES-256-CTR key (bytes 8–40)
// and IV (40–56) — a secret is only mixed in for MTProxy servers, which
// are not what a DC route sees — and bytes 56–60, once decrypted, are the
// transport tag.

// mtprotoInitLen is the obfuscated2 header length.
const mtprotoInitLen = 64

// Transport tags a decrypted obfuscated2 header carries at bytes 56–60.
const (
	mtprotoTagAbridged     = 0xefefefef
	mtprotoTagIntermediate = 0xeeeeeeee
	mtprotoTagPadded       = 0xdddddddd
)

// looksLikeMTProto reports whether b opens an obfuscated2 MTProto stream.
func looksLikeMTProto(b []byte) bool {
	if len(b) < mtprotoInitLen {
		return false
	}
	block, err := aes.NewCipher(b[8:40])
	if err != nil {
		return false
	}
	var ks [mtprotoInitLen]byte
	cipher.NewCTR(block, b[40:56]).XORKeyStream(ks[:], b[:mtprotoInitLen])
	switch binary.LittleEndian.Uint32(ks[56:60]) {
	case mtprotoTagAbridged, mtprotoTagIntermediate, mtprotoTagPadded:
		return true
	}
	return false
}
