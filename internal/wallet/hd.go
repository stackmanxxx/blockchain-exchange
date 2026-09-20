package wallet

import (
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/tyler-smith/go-bip32"
	"github.com/tyler-smith/go-bip39"
)

// BIP44 coin type（slip-0044）
const (
	CoinTypeBTC = 0
	CoinTypeETH = 60
	CoinTypeTRX = 195
)

// HDWallet BIP39 助记词 + BIP44 分层派生钱包。
// 生产环境：助记词/根密钥应存 KMS/HSM，进程内不落盘。
type HDWallet struct {
	mnemonic string
	master   *bip32.Key
}

// GenerateMnemonic 生成 24 词助记词
func GenerateMnemonic() (string, error) {
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return "", err
	}
	return bip39.NewMnemonic(entropy)
}

// NewHDWallet 从助记词构造钱包
func NewHDWallet(mnemonic string) (*HDWallet, error) {
	if !bip39.IsMnemonicValid(mnemonic) {
		return nil, fmt.Errorf("wallet: invalid mnemonic")
	}
	seed := bip39.NewSeed(mnemonic, "")
	master, err := bip32.NewMasterKey(seed)
	if err != nil {
		return nil, err
	}
	return &HDWallet{mnemonic: mnemonic, master: master}, nil
}

// derive 按 BIP44 路径 m/44'/coinType'/account'/change/index 派生
func (w *HDWallet) derive(coinType, account, change, index uint32) (*bip32.Key, error) {
	key := w.master
	for _, v := range []uint32{
		bip32.FirstHardenedChild + 44,
		bip32.FirstHardenedChild + coinType,
		bip32.FirstHardenedChild + account,
		change,
		index,
	} {
		var err error
		key, err = key.NewChildKey(v)
		if err != nil {
			return nil, err
		}
	}
	return key, nil
}

// deriveETH 派生 ETH/EVM 地址与私钥（coinType 60）
func (w *HDWallet) deriveETH(account, index uint32) (addr string, priv []byte, err error) {
	key, err := w.derive(CoinTypeETH, account, 0, index)
	if err != nil {
		return "", nil, err
	}
	if !key.IsPrivate {
		return "", nil, fmt.Errorf("wallet: derived public key (not private)")
	}
	// go-bip32 v1：派生的 Key.Key 为 32 字节原始私钥
	ecdsaPriv, err := crypto.ToECDSA(key.Key)
	if err != nil {
		return "", nil, err
	}
	address := crypto.PubkeyToAddress(ecdsaPriv.PublicKey)
	return address.Hex(), crypto.FromECDSA(ecdsaPriv), nil
}

// deriveBTC 派生 BTC P2PKH 地址与 WIF 私钥（coinType 0）
func (w *HDWallet) deriveBTC(account, index uint32, net *chaincfg.Params) (addr string, wifPriv []byte, err error) {
	key, err := w.derive(CoinTypeBTC, account, 0, index)
	if err != nil {
		return "", nil, err
	}
	if !key.IsPrivate {
		return "", nil, fmt.Errorf("wallet: derived public key (not private)")
	}
	privKey, _ := btcec.PrivKeyFromBytes(key.Key)
	hash := address.Hash160(privKey.PubKey().SerializeCompressed())
	btcAddr, err := address.NewAddressPubKeyHash(hash, net)
	if err != nil {
		return "", nil, err
	}
	wif, err := btcutil.NewWIF(privKey, net, true)
	if err != nil {
		return "", nil, err
	}
	return btcAddr.EncodeAddress(), []byte(wif.String()), nil
}

// deriveTRX 派生 TRON 地址与私钥（coinType 195）
// TRON 地址 = base58check(0x41 + keccak256(pubkey) 后 20 字节)，私钥格式同 ETH
func (w *HDWallet) deriveTRX(account, index uint32) (addr string, priv []byte, err error) {
	key, err := w.derive(CoinTypeTRX, account, 0, index)
	if err != nil {
		return "", nil, err
	}
	if !key.IsPrivate {
		return "", nil, fmt.Errorf("wallet: derived public key (not private)")
	}
	ecdsaPriv, err := crypto.ToECDSA(key.Key)
	if err != nil {
		return "", nil, err
	}
	ethAddrBytes := crypto.PubkeyToAddress(ecdsaPriv.PublicKey).Bytes() // 20 字节
	tron21 := append([]byte{0x41}, ethAddrBytes...)
	return b58CheckEncode(tron21), crypto.FromECDSA(ecdsaPriv), nil
}

// DeriveAddress 按币种派生充值地址与热钱包私钥。
// 返回：address、私钥字节（生产由 KMS 托管，此处为 HD 派生结果）
func (w *HDWallet) DeriveAddress(assetSymbol string, account, index uint32) (addr string, priv []byte, err error) {
	switch assetSymbol {
	case "ETH", "USDT_ERC20", "USDC", "BSC", "POLYGON":
		return w.deriveETH(account, index)
	case "BTC":
		return w.deriveBTC(account, index, &chaincfg.MainNetParams)
	case "USDT_TRC20", "TRX":
		return w.deriveTRX(account, index)
	default:
		return "", nil, fmt.Errorf("wallet: unsupported asset %s", assetSymbol)
	}
}

// SignEVM 用 ECDSA 私钥字节构造签名者（EVM 交易签名用）
func SignEVM(privBytes []byte) (*ecdsa.PrivateKey, error) {
	priv, err := crypto.ToECDSA(privBytes)
	if err != nil {
		return nil, fmt.Errorf("wallet: invalid evm private key: %w", err)
	}
	return priv, nil
}

// _ 编译期保证
var _ = hex.EncodeToString
