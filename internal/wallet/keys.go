package wallet

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"github.com/blakeswap/blakeswap/internal/chain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/tyler-smith/go-bip39"
	"strconv"
)

type Keys struct {
	master  *hdkeychain.ExtendedKey
	network chain.Network
}

func NewMnemonic() (string, error) {
	entropy, e := bip39.NewEntropy(256)
	if e != nil {
		return "", e
	}
	return bip39.NewMnemonic(entropy)
}
func FromMnemonic(m string) (*Keys, error) {
	if !bip39.IsMnemonicValid(m) {
		return nil, errors.New("invalid BIP39 mnemonic")
	}
	k, e := hdkeychain.NewMaster(bip39.NewSeed(m, ""), &chaincfg.RegressionNetParams)
	if e != nil {
		return nil, e
	}
	return &Keys{master: k}, nil
}

// Private hardened branches separate spending and app identity. Context is hashed
// into eight hardened path components (248 bits); no public derivation is exposed.
func (k *Keys) Derive(branch uint32, context string) (*btcec.PrivateKey, error) {
	d := sha256.Sum256([]byte("blakeswap/v1/" + k.network.KeyContext(context)))
	path := []uint32{83696968, branch}
	for i := 0; i < 8; i++ {
		path = append(path, binary.BigEndian.Uint32(d[i*4:i*4+4])&0x7fffffff)
	}
	current := k.master
	for _, n := range path {
		next, e := current.Derive(n + hdkeychain.HardenedKeyStart)
		if e != nil {
			return nil, e
		}
		current = next
	}
	return current.ECPrivKey()
}
func (k *Keys) Spending(id chain.ID, context string) (*btcec.PrivateKey, error) {
	if !id.Valid() {
		return nil, errors.New("unknown chain")
	}
	branch := uint32(0)
	if id == chain.Blake {
		branch = 1
	}
	return k.Derive(branch, context)
}

// Receive preserves the original deposit key at index zero for existing wallets.
func (k *Keys) Receive(id chain.ID, index uint32) (*btcec.PrivateKey, error) {
	context := "deposit"
	if index > 0 {
		context += "/" + strconv.FormatUint(uint64(index), 10)
	}
	return k.Spending(id, context)
}
func (k *Keys) SetNetwork(n chain.Network)                 { k.network = n.Normalized() }
func Address(pub *btcec.PublicKey) (string, []byte, error) { return AddressFor(chain.Regtest, pub) }
func AddressFor(n chain.Network, pub *btcec.PublicKey) (string, []byte, error) {
	addr, e := btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(pub.SerializeCompressed()), n.Params())
	if e != nil {
		return "", nil, e
	}
	s, e := txscript.PayToAddrScript(addr)
	return addr.EncodeAddress(), s, e
}
