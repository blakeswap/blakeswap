package chain

import (
	"fmt"
	"github.com/btcsuite/btcd/chaincfg"
)

// Network identifies consensus rules, not merely address encoding. Testnet means
// Testnet4 on both chains; Testnet3 and signet are deliberately not aliases.
type Network string

const (
	Regtest Network = "regtest"
	Testnet Network = "testnet"
	Mainnet Network = "mainnet"
)

func (n Network) Normalized() Network {
	if n == "" {
		return Regtest
	}
	return n
}
func (n Network) Valid() bool {
	n = n.Normalized()
	return n == Regtest || n == Testnet || n == Mainnet
}
func (n Network) NodeName() string {
	switch n.Normalized() {
	case Mainnet:
		return "main"
	case Testnet:
		return "testnet4"
	default:
		return "regtest"
	}
}
func (n Network) ForkHeight() uint32 {
	switch n.Normalized() {
	case Mainnet:
		return 961640
	case Testnet:
		return 150308
	default:
		return 1
	}
}
func (n Network) Genesis() string {
	switch n.Normalized() {
	case Mainnet:
		return "000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f"
	case Testnet:
		return "00000000da84f2bafbbc53dee25a72ae507ff4914b867c565be350b0da8bf043"
	default:
		return "0f9188f13cb7b2c71f2a335e3a4fc328bf5beb436012afca590b1a11466e2206"
	}
}
func (n Network) Params() *chaincfg.Params {
	switch n.Normalized() {
	case Mainnet:
		return &chaincfg.MainNetParams
	case Testnet:
		return &chaincfg.TestNet3Params
	default:
		return &chaincfg.RegressionNetParams
	}
}                                   // Testnet4 uses the same address encoding as Testnet3.
func (n Network) Namespace() string { return "blakeswap-" + string(n.Normalized()) + "-v1" }
func (n Network) KeyContext(context string) string {
	if n.Normalized() == Regtest {
		return context
	}
	return string(n) + "/" + context
}
func (n Network) Domain(id ID) string {
	if id == BTC {
		return fmt.Sprintf("bitcoin:%s:bip143", n.Normalized())
	}
	return fmt.Sprintf("bitcoin-blake2b:%s:activation%d:unified21", n.Normalized(), n.ForkHeight())
}
func (n Network) Confirmations() int {
	if n.Normalized() == Regtest {
		return 2
	}
	return 6
}
func (n Network) HorizonScale() uint32 {
	if n.Normalized() == Regtest {
		return 1
	}
	return 3
}
