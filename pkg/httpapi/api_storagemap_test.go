package httpapi

import "testing"

func TestParseUgnotPerByte(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int64
		err  bool
	}{
		{"mainnet's value", `"100ugnot"`, 100, false},
		{"a governance change", `"250ugnot"`, 250, false},
		{"free storage is a real setting", `"0ugnot"`, 0, false},
		// A price in another denom makes the capacity a number of something
		// else. Better to fall back and label it than to divide by 5.
		{"another denom is refused, not read for digits", `"5foo"`, 0, true},
		{"a bare number has no denom to check", `"100"`, 0, true},
		{"params are JSON-encoded, so unquoted is malformed", `100ugnot`, 0, true},
		{"empty", `""`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseUgnotPerByte(tt.raw)
			if (err != nil) != tt.err {
				t.Fatalf("parseUgnotPerByte(%s) error = %v, want error = %v", tt.raw, err, tt.err)
			}
			if err == nil && got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCapacityBytes(t *testing.T) {
	tests := []struct {
		name   string
		supply string
		price  int64
		want   string
		ok     bool
	}{
		// mainnet at 2026-09-21. The monorepo does this same sum in a comment
		// next to storagePriceDefault and calls it 13.33 TB.
		{"mainnet", "1333000221686563", 100, "13330002216865", true},
		{"a halved price doubles the disk", "1333000221686563", 50, "26660004433731", true},
		// int64 would hold this, but the inputs are a whole money supply over a
		// governance-controlled divisor, so the arithmetic is big.Int.
		{"a supply past int64", "99999999999999999999999999", 100, "999999999999999999999999", true},
		{"free storage is an unbounded disk, not a division", "1333000221686563", 0, "", false},
		{"a negative price is not a price", "1333000221686563", -100, "", false},
		{"an unparseable supply yields no capacity", "not-a-number", 100, "", false},
		{"an absent supply yields no capacity", "", 100, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := capacityBytes(tt.supply, tt.price)
			if ok != tt.ok {
				t.Fatalf("capacityBytes(%q, %d) ok = %v, want %v", tt.supply, tt.price, ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}
