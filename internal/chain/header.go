package chain

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/big"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"golang.org/x/crypto/blake2b"
)

// HeaderHash follows Bitcoin Knots 29.4.1 src/primitives/block.cpp. The serialized
// 164-byte header is not itself the Blake2b PoW preimage.
func HeaderHash(h []byte) (chainhash.Hash, error) {
	if len(h) == 80 {
		return chainhash.DoubleHashH(h), nil
	}
	if len(h) != 164 {
		return chainhash.Hash{}, errors.New("invalid header size")
	}
	tag := func(name string, parts ...[]byte) []byte {
		prefix := sha256.Sum256([]byte(name))
		sum := sha256.New()
		sum.Write(prefix[:])
		sum.Write(prefix[:])
		for _, p := range parts {
			sum.Write(p)
		}
		return sum.Sum(nil)
	}
	prev := append([]byte(nil), h[4:36]...)
	for i := 0; i < 16; i++ {
		prev[i], prev[31-i] = prev[31-i], prev[i]
	}
	xor := h[112:128]
	mask := make([]byte, 32)
	nonzero := false
	for _, b := range xor {
		nonzero = nonzero || b != 0
	}
	if nonzero {
		mask = tag("Bitcoin block hash PoW XOR mask", xor)
		clearBits := int(h[111])
		clear(mask[:clearBits/8])
		mask[clearBits/8] &= byte(0xff >> uint(clearBits%8))
	}
	count := make([]byte, 4)
	binary.LittleEndian.PutUint32(count, uint32(binary.LittleEndian.Uint16(h[108:110])))
	h1 := tag("Bitcoin block header 1", h[:4], prev, h[128:132], h[36:68], h[68:72], []byte{0}, h[72:76], count, h[110:112], tag("Bitcoin block hash PoW XOR key", xor))
	h2 := tag("Merge-mining hook", h1, make([]byte, 32), h[132:164])
	preimage := append(make([]byte, 4), h2...)
	preimage = append(preimage, h[88:104]...)
	extra := blake2b.Sum256(preimage)
	var grind []byte
	switch h[110] & 3 {
	case 3:
		grind = make([]byte, 32)
		fallthrough
	case 2:
		grind = append(grind, make([]byte, 48)...)
		grind = append(grind, h2...)
	case 0:
		grind = tag("Bitcoin prevblock header, hashed", prev)
		clear(grind[:6])
	}
	grind = append(grind, h[76:84]...)
	if h[110]&3 == 1 {
		grind = append(grind, h[84:88]...)
		grind = append(grind, h[104:108]...)
	} else {
		grind = append(grind, h[104:108]...)
		grind = append(grind, h[84:88]...)
	}
	grind = append(grind, extra[:]...)
	if h[110]&3 == 1 {
		grind = append(grind, h2...)
	}
	result := blake2b.Sum256(grind)
	var out chainhash.Hash
	for i, b := range result {
		out[31-i] = b ^ mask[i]
	}
	return out, nil
}
func verifyHeaderWork(h []byte, network Network) error {
	hash, err := HeaderHash(h)
	if err != nil {
		return err
	}
	bits := binary.LittleEndian.Uint32(h[72:76])
	mantissa := bits & 0x007fffff
	exponent := uint(bits >> 24)
	if mantissa == 0 || bits&0x00800000 != 0 || exponent > 34 {
		return errors.New("invalid proof of work target")
	}
	target := new(big.Int).SetUint64(uint64(mantissa))
	if exponent <= 3 {
		target.Rsh(target, 8*(3-exponent))
	} else {
		target.Lsh(target, 8*(exponent-3))
	}
	if target.Sign() <= 0 || target.BitLen() > 256 {
		return errors.New("invalid proof of work target")
	}
	// Both algorithms use the network's consensus limit, including after the
	// Blake2b activation target shift. Testnet4 shares Testnet3's PoW limit.
	if !network.Valid() || target.Cmp(network.Params().PowLimit) > 0 {
		return errors.New("proof of work target exceeds network limit")
	}
	value, _ := new(big.Int).SetString(hash.String(), 16)
	if value.Cmp(target) > 0 {
		return errors.New("invalid header proof of work")
	}
	return nil
}
