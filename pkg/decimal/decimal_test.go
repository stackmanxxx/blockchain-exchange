package decimal

import "testing"

func TestFromStringAndString(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0", "0"},
		{"1.5", "1.5"},
		{"0.00000001", "0.00000001"},
		{"123.45678901", "123.45678901"},
		{"-42.5", "-42.5"},
		{"100000000", "100000000"},
	}
	for _, c := range cases {
		d, err := FromString(c.in)
		if err != nil {
			t.Fatalf("FromString(%q): %v", c.in, err)
		}
		if got := d.String(); got != c.want {
			t.Fatalf("FromString(%q).String() = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFromStringInvalid(t *testing.T) {
	for _, in := range []string{"", "abc", "1.234567891", "1..2", "1.2.3"} {
		if _, err := FromString(in); err == nil {
			t.Fatalf("FromString(%q) should fail", in)
		}
	}
}

func TestAddSub(t *testing.T) {
	a, _ := FromString("1.5")
	b, _ := FromString("2.25")
	if got := a.Add(b).String(); got != "3.75" {
		t.Fatalf("add = %s", got)
	}
	if got := b.Sub(a).String(); got != "0.75" {
		t.Fatalf("sub = %s", got)
	}
}

func TestMul(t *testing.T) {
	// 3.5 USDT × 2 BTC = 7.0 USDT
	price, _ := FromString("3.5")
	qty, _ := FromString("2")
	if got := price.Mul(qty).String(); got != "7" {
		t.Fatalf("mul = %s, want 7", got)
	}
	// 0.33333333 × 3 = 0.99999999（向下取整 8 位）
	a, _ := FromString("0.33333333")
	b, _ := FromString("3")
	if got := a.Mul(b).String(); got != "0.99999999" {
		t.Fatalf("mul2 = %s", got)
	}
}

func TestUnits(t *testing.T) {
	d := FromUnits(123_456_789)
	if got := d.String(); got != "1.23456789" {
		t.Fatalf("units string = %s", got)
	}
	if FromUnits(0).IsZero() != true || !FromUnits(5).IsPositive() {
		t.Fatal("predicates wrong")
	}
}
