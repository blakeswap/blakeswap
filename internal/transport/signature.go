package transport

import (
	"crypto/sha256"
	"errors"
	"fiatjaf.com/nostr"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"strconv"
	"unicode/utf8"
)

// EventID uses the NIP-01 canonical JSON array with ordinary bounds-checked Go.
// The pinned Nostr dependency's optimized streaming serializer uses uintptr
// arithmetic that fails Go's checkptr instrumentation; we do not call it.
func EventID(event nostr.Event) nostr.ID {
	b := []byte(`[0,`)
	b = appendJSONString(b, event.PubKey.Hex())
	b = append(b, ',')
	b = strconv.AppendInt(b, int64(event.CreatedAt), 10)
	b = append(b, ',')
	b = strconv.AppendInt(b, int64(event.Kind), 10)
	b = append(b, ',', '[')
	for i, tag := range event.Tags {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '[')
		for j, s := range tag {
			if j > 0 {
				b = append(b, ',')
			}
			b = appendJSONString(b, s)
		}
		b = append(b, ']')
	}
	b = append(b, ']', ',')
	b = appendJSONString(b, event.Content)
	b = append(b, ']')
	return nostr.ID(sha256.Sum256(b))
}
func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	const hex = "0123456789abcdef"
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '"', '\\':
			b = append(b, '\\', ch)
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		case '\b':
			b = append(b, '\\', 'b')
		case '\f':
			b = append(b, '\\', 'f')
		default:
			if ch < 32 {
				b = append(b, '\\', 'u', '0', '0', hex[ch>>4], hex[ch&15])
			} else {
				b = append(b, ch)
			}
		}
	}
	return append(b, '"')
}
func validStrings(event nostr.Event) bool {
	if !utf8.ValidString(event.Content) {
		return false
	}
	for _, tag := range event.Tags {
		for _, s := range tag {
			if !utf8.ValidString(s) {
				return false
			}
		}
	}
	return true
}
func Sign(event *nostr.Event, key nostr.SecretKey) error {
	if !validStrings(*event) {
		return errors.New("invalid UTF-8 event")
	}
	var scalar btcec.ModNScalar
	if scalar.SetByteSlice(key[:]) || scalar.IsZero() {
		return errors.New("invalid identity private key")
	}
	if event.Tags == nil {
		event.Tags = nostr.Tags{}
	}
	event.PubKey = key.Public()
	event.ID = EventID(*event)
	private, _ := btcec.PrivKeyFromBytes(key[:])
	signature, err := schnorr.Sign(private, event.ID[:])
	if err != nil {
		return err
	}
	event.Sig = [64]byte(signature.Serialize())
	return nil
}
