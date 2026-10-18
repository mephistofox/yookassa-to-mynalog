package receipts

import (
	"encoding/json"
	"testing"
)

func TestIncomeMoneyAcceptsExactFNSNumberFormats(t *testing.T) {
	for _, test := range []struct {
		input string
		want  Money
	}{
		{`420`, 42000}, {`420.0`, 42000}, {`"420.00"`, 42000}, {`4.2E+2`, 42000}, {`4.2000e+2`, 42000}, {`"4.2E+2"`, 42000},
		{`1E-2`, 1}, {`1.234e2`, 12340}, {`100.10`, 10010}, {`0e+0`, 0}, {`9.999999999999999e13`, 9999999999999999},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := (Income{Total: json.RawMessage(test.input)}).Money()
			if err != nil || got != test.want {
				t.Fatalf("amount=%q got=%v want=%v err=%v", test.input, got, test.want, err)
			}
		})
	}
}
func TestIncomeMoneyRejectsFractionalKopecksOverflowAndMalformed(t *testing.T) {
	for _, input := range []string{`0.001`, `1e-3`, `-1`, `-0.00`, `1e14`, `1e999999999`, `1e-999999999`, `1e21`, `1e-21`, `null`, `true`, `{}`, `[]`, `"1/2"`, `"0x10"`, `"+1"`, `"NaN"`, `"1p2"`, `"01"`, `" 1.00"`, `"1.00"junk`, ``} {
		t.Run(input, func(t *testing.T) {
			if m, err := (Income{Total: json.RawMessage(input)}).Money(); err == nil {
				t.Fatalf("unsafe amount accepted %q=%v", input, m)
			}
		})
	}
}
func TestYooKassaMoneyRemainsStrictAfterFNSNumberSupport(t *testing.T) {
	for _, input := range []string{"4.2E+2", "420", "420.0", "1.001"} {
		if _, err := ParseMoney(input); err == nil {
			t.Fatalf("provider amount relaxed %q", input)
		}
	}
}
