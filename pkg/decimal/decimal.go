// Package decimal 封装金额运算，内部统一使用最小精度整数（如 1e8）避免浮点误差。
// 对外 API 层使用字符串展示，业务层使用 *big.Int 最小单位运算。
package decimal

import (
	"errors"
	"math/big"
	"strings"
)

// 全局精度：1e8（与主流交易所一致，8 位小数）
const Precision = 8
const Base = 100_000_000

var (
	ErrInvalid = errors.New("decimal: invalid value")
	baseInt    = big.NewInt(Base)
)

// Decimal 以最小单位整数表示金额，支持 8 位小数精度
type Decimal struct {
	// 最小单位值，如 1.5 USDT = 150000000
	Units *big.Int
}

// FromUnits 从最小单位整数构造
func FromUnits(units int64) Decimal {
	return Decimal{Units: big.NewInt(units)}
}

// FromString 从十进制字符串构造（支持最多 8 位小数）
func FromString(s string) (Decimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Decimal{}, ErrInvalid
	}
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	} else if s[0] == '+' {
		s = s[1:]
	}
	parts := strings.SplitN(s, ".", 2)
	intPart := parts[0]
	fracPart := ""
	if len(parts) == 2 {
		fracPart = parts[1]
	}
	if intPart == "" && fracPart == "" {
		return Decimal{}, ErrInvalid
	}
	if intPart == "" {
		intPart = "0"
	}
	if len(fracPart) > Precision {
		return Decimal{}, ErrInvalid
	}
	for len(fracPart) < Precision {
		fracPart += "0"
	}
	combined := intPart + fracPart
	units, ok := new(big.Int).SetString(combined, 10)
	if !ok {
		return Decimal{}, ErrInvalid
	}
	if neg {
		units.Neg(units)
	}
	return Decimal{Units: units}, nil
}

// String 输出十进制字符串（整数不带小数部分，如 "7" 而非 "7.0"）
func (d Decimal) String() string {
	if d.Units == nil {
		return "0"
	}
	neg := d.Units.Sign() < 0
	abs := new(big.Int).Abs(d.Units)
	str := abs.String()
	if len(str) <= Precision {
		padding := strings.Repeat("0", Precision-len(str)+1)
		str = padding + str
	}
	intPart := str[:len(str)-Precision]
	fracPart := strings.TrimRight(str[len(str)-Precision:], "0")
	if fracPart == "" {
		// 整数（含 0）：不带小数部分
		if neg && intPart != "0" {
			return "-" + intPart
		}
		return intPart
	}
	result := intPart + "." + fracPart
	if neg {
		result = "-" + result
	}
	return result
}

// Add 加法
func (d Decimal) Add(o Decimal) Decimal {
	return Decimal{Units: new(big.Int).Add(d.Units, o.Units)}
}

// Sub 减法
func (d Decimal) Sub(o Decimal) Decimal {
	return Decimal{Units: new(big.Int).Sub(d.Units, o.Units)}
}

// MulInt 乘以整数
func (d Decimal) MulInt(n int64) Decimal {
	return Decimal{Units: new(big.Int).Mul(d.Units, big.NewInt(n))}
}

// Cmp 比较：-1 小于，0 等于，1 大于
func (d Decimal) Cmp(o Decimal) int {
	return d.Units.Cmp(o.Units)
}

// IsZero 是否为零
func (d Decimal) IsZero() bool {
	return d.Units.Sign() == 0
}

// IsPositive 是否为正
func (d Decimal) IsPositive() bool {
	return d.Units.Sign() > 0
}

// IsNegative 是否为负
func (d Decimal) IsNegative() bool {
	return d.Units.Sign() < 0
}

// Mul 金额相乘（价格 × 数量），结果保留 8 位小数
// 例如 price=3.5 USDT (350000000), qty=2 BTC (200000000)
// 结果 = 350000000*200000000 / 1e8 = 700000000 (7.0 USDT)
func (d Decimal) Mul(o Decimal) Decimal {
	num := new(big.Int).Mul(d.Units, o.Units)
	units := num.Div(num, baseInt)
	return Decimal{Units: units}
}

// PriceQtyToQuote 由价格与数量（均为最小单位）计算 quote 金额（最小单位）：
// quote = price * qty / 1e8，向下取整；溢出 int64 返回错误。
// 这是撮合/冻结/清算共用的金额换算，避免各模块重复实现。
func PriceQtyToQuote(priceUnits, qtyUnits int64) (int64, error) {
	if priceUnits <= 0 || qtyUnits <= 0 {
		return 0, nil
	}
	num := new(big.Int).Mul(big.NewInt(priceUnits), big.NewInt(qtyUnits))
	num.Div(num, baseInt)
	if !num.IsInt64() {
		return 0, ErrInvalid
	}
	return num.Int64(), nil
}
