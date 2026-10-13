package egress

import (
	"reflect"
	"testing"
)

func TestNormalizeAllowedIPsCanonicalizesAndSorts(t *testing.T) {
	tests := []struct {
		name    string
		values  []string
		want    []string
		wantErr bool
	}{
		{name: "empty list stays nil", values: nil, want: nil},
		{
			name:   "an address becomes its single-address range",
			values: []string{"203.0.113.7"},
			want:   []string{"203.0.113.7/32"},
		},
		{
			name:   "a range is masked and a duplicate is dropped",
			values: []string{"10.0.0.5/8", "10.1.2.3/8", "10.0.0.0/8"},
			want:   []string{"10.0.0.0/8"},
		},
		{
			name:   "entries are sorted independently of declaration order",
			values: []string{"198.51.100.0/24", "203.0.113.7", "10.0.0.0/8"},
			want:   []string{"10.0.0.0/8", "198.51.100.0/24", "203.0.113.7/32"},
		},
		{
			name:   "an ipv6 range is accepted",
			values: []string{"2001:db8::1"},
			want:   []string{"2001:db8::1/128"},
		},
		{name: "a name is rejected", values: []string{"api.example.com"}, wantErr: true},
		{name: "a wildcard is rejected", values: []string{"10.0.0.*"}, wantErr: true},
		{name: "an empty entry is rejected", values: []string{" "}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeAllowedIPs(test.values)
			if test.wantErr {
				if err == nil {
					t.Fatalf("NormalizeAllowedIPs(%v) = %v, want an error", test.values, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeAllowedIPs(%v) error = %v", test.values, err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("NormalizeAllowedIPs(%v) = %v, want %v", test.values, got, test.want)
			}
		})
	}
}

func TestAddressAllowedFailsClosed(t *testing.T) {
	allowed := []string{"10.0.0.0/8", "203.0.113.7/32"}
	tests := []struct {
		name    string
		address string
		want    bool
	}{
		{name: "inside a range", address: "10.1.2.3", want: true},
		{name: "the range's own address", address: "10.0.0.0", want: true},
		{name: "the single allowed address", address: "203.0.113.7", want: true},
		{name: "outside every range", address: "198.51.100.7"},
		{name: "just outside the single address", address: "203.0.113.8"},
		{name: "not an address at all", address: "api.example.com"},
		{name: "empty", address: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := AddressAllowed(test.address, allowed); got != test.want {
				t.Fatalf("AddressAllowed(%q) = %v, want %v", test.address, got, test.want)
			}
		})
	}
	if AddressAllowed("10.0.0.1", []string{"not-a-range"}) {
		t.Fatal("AddressAllowed accepted an address against an unparsable range")
	}
}

func TestIsIPLiteral(t *testing.T) {
	tests := map[string]bool{
		"10.0.0.5":        true,
		"203.0.113.7":     true,
		"::1":             true,
		"[2001:db8::1]":   true,
		"api.example.com": false,
		"*.example.com":   false,
		"10.0.0.*":        false,
	}
	for value, want := range tests {
		if got := IsIPLiteral(value); got != want {
			t.Fatalf("IsIPLiteral(%q) = %v, want %v", value, got, want)
		}
	}
}
