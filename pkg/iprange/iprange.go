// Package iprange parses IPv4 addresses and inclusive "start-end" ranges.
package iprange

import (
	"fmt"
	"net/netip"
	"strings"
)

// Range is an inclusive IPv4 address range.
type Range struct {
	Start netip.Addr
	End   netip.Addr
}

func (r Range) Contains(ip netip.Addr) bool {
	return ip.Is4() && r.Start.Compare(ip) <= 0 && ip.Compare(r.End) <= 0
}

// List is a set of ranges in the order they were declared.
type List []Range

// ParseIPv4 parses a single IPv4 address. IPv4-mapped IPv6 and zoned addresses are rejected.
func ParseIPv4(s string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid IP %q: %w", s, err)
	}
	if !ip.Is4() {
		return netip.Addr{}, fmt.Errorf("%q is not an IPv4 address", s)
	}
	return ip, nil
}

// ParseRange parses "a.b.c.d" or "a.b.c.d-e.f.g.h".
func ParseRange(s string) (Range, error) {
	startStr, endStr, isRange := strings.Cut(s, "-")
	start, err := ParseIPv4(startStr)
	if err != nil {
		return Range{}, err
	}
	if !isRange {
		return Range{Start: start, End: start}, nil
	}
	end, err := ParseIPv4(endStr)
	if err != nil {
		return Range{}, err
	}
	if end.Less(start) {
		return Range{}, fmt.Errorf("invalid range %q: start is greater than end", s)
	}
	return Range{Start: start, End: end}, nil
}

// Parse parses all items and fails on the first invalid one.
func Parse(items []string) (List, error) {
	l := make(List, 0, len(items))
	for _, item := range items {
		r, err := ParseRange(item)
		if err != nil {
			return nil, err
		}
		l = append(l, r)
	}
	return l, nil
}

func (l List) Contains(ip netip.Addr) bool {
	for _, r := range l {
		if r.Contains(ip) {
			return true
		}
	}
	return false
}

// First returns the first address of the first range.
func (l List) First() (netip.Addr, bool) {
	if len(l) == 0 {
		return netip.Addr{}, false
	}
	return l[0].Start, true
}
