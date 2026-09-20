package wallet

import (
	"crypto/sha256"
	"fmt"
	"math/big"
)

// Bitcoin/TRON base58 字母表（无 0OIl）
const b58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// b58Encode 标准 base58 编码
func b58Encode(input []byte) string {
	x := new(big.Int).SetBytes(input)
	base := big.NewInt(58)
	zero := big.NewInt(0)
	mod := new(big.Int)
	out := make([]byte, 0, len(input)+len(input)/2)
	for x.Cmp(zero) > 0 {
		x.DivMod(x, base, mod)
		out = append(out, b58Alphabet[mod.Int64()])
	}
	// 前导零字节 → '1'
	for _, b := range input {
		if b != 0 {
			break
		}
		out = append(out, '1')
	}
	// 反转
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// b58Decode base58 解码
func b58Decode(s string) ([]byte, error) {
	x := big.NewInt(0)
	base := big.NewInt(58)
	for i := 0; i < len(s); i++ {
		idx := indexOfB58(s[i])
		if idx < 0 {
			return nil, fmt.Errorf("wallet: invalid base58 char %q", s[i])
		}
		x.Mul(x, base)
		x.Add(x, big.NewInt(int64(idx)))
	}
	out := x.Bytes()
	// 前导 '1' → 零字节
	zeros := 0
	for i := 0; i < len(s) && s[i] == '1'; i++ {
		zeros++
	}
	prefix := make([]byte, zeros)
	return append(prefix, out...), nil
}

func indexOfB58(c byte) int {
	for i := 0; i < len(b58Alphabet); i++ {
		if b58Alphabet[i] == c {
			return i
		}
	}
	return -1
}

// b58CheckEncode base58check 编码（payload + 4 字节校验和）
func b58CheckEncode(payload []byte) string {
	checksum := sha256.Sum256(payload)
	checksum = sha256.Sum256(checksum[:])
	full := append(append([]byte{}, payload...), checksum[:4]...)
	return b58Encode(full)
}

// b58CheckDecode base58check 解码并校验
func b58CheckDecode(s string) ([]byte, error) {
	full, err := b58Decode(s)
	if err != nil {
		return nil, err
	}
	if len(full) < 5 {
		return nil, fmt.Errorf("wallet: base58check too short")
	}
	payload, checksum := full[:len(full)-4], full[len(full)-4:]
	calc := sha256.Sum256(payload)
	calc = sha256.Sum256(calc[:])
	for i := 0; i < 4; i++ {
		if checksum[i] != calc[i] {
			return nil, fmt.Errorf("wallet: base58check checksum mismatch")
		}
	}
	return payload, nil
}
