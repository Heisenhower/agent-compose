package egress

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// ParseAllowedIP normalizes one declared allowed address into a canonical CIDR.
// A bare address becomes its single-address prefix, so a declaration may name an
// address or a range and both end up in the one representation the decision
// compares against.
//
// Only literal addresses are accepted. A name here would have to be resolved,
// and an allowance that depends on a second resolution is exactly the indirection
// allowed_ips exists to remove.
func ParseAllowedIP(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("allowed address must not be empty")
	}
	if prefix, err := netip.ParsePrefix(trimmed); err == nil {
		return prefix.Masked().String(), nil
	}
	address, err := netip.ParseAddr(trimmed)
	if err != nil {
		return "", fmt.Errorf("allowed address %q must be an IP address or a CIDR range", trimmed)
	}
	return netip.PrefixFrom(address, address.BitLen()).String(), nil
}

// NormalizeAllowedIPs validates a declared allowed-address list, canonicalizes
// every entry, rejects duplicates, and sorts the result so the same declaration
// always compiles to the same policy generation. A nil list stays nil: an
// allowance with no address constraint is a different declaration from one whose
// list is empty after validation.
func NormalizeAllowedIPs(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		canonical, err := ParseAllowedIP(value)
		if err != nil {
			return nil, fmt.Errorf("allowed_ips[%d]: %w", index, err)
		}
		if _, duplicate := seen[canonical]; duplicate {
			continue
		}
		seen[canonical] = struct{}{}
		normalized = append(normalized, canonical)
	}
	slices.Sort(normalized)
	return normalized, nil
}

// AddressAllowed reports whether one address falls inside any of the canonical
// CIDRs. An unparsable address or prefix is not allowed: a constraint the engine
// cannot evaluate must fail closed.
func AddressAllowed(address string, allowed []string) bool {
	parsed, err := netip.ParseAddr(strings.TrimSpace(address))
	if err != nil {
		return false
	}
	for _, candidate := range allowed {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(candidate))
		if err != nil {
			continue
		}
		if prefix.Contains(parsed.Unmap()) {
			return true
		}
	}
	return false
}

// IsIPLiteral reports whether a host names an address rather than a name. The
// decision uses it to keep wildcard patterns from reaching an address: a pattern
// matches names, and an address is matched by the same address.
func IsIPLiteral(host string) bool {
	_, err := netip.ParseAddr(strings.Trim(strings.TrimSpace(host), "[]"))
	return err == nil
}
