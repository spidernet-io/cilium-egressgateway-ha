package iprange

import (
	"net/netip"
	"testing"
)

func TestParseRange(t *testing.T) {
	tests := []struct {
		in      string
		start   string
		end     string
		wantErr bool
	}{
		{in: "192.168.10.100", start: "192.168.10.100", end: "192.168.10.100"},
		{in: "192.168.10.100-192.168.10.110", start: "192.168.10.100", end: "192.168.10.110"},
		{in: " 10.0.0.1 - 10.0.0.1 ", start: "10.0.0.1", end: "10.0.0.1"},
		{in: "192.168.10.110-192.168.10.100", wantErr: true},
		{in: "fd00::1", wantErr: true},
		{in: "::ffff:10.0.0.1", wantErr: true},
		{in: "10.0.0.0/24", wantErr: true},
		{in: "10.0.0.1-", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		r, err := ParseRange(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseRange(%q) = %v, want error", tt.in, r)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRange(%q) unexpected error: %v", tt.in, err)
			continue
		}
		if r.Start.String() != tt.start || r.End.String() != tt.end {
			t.Errorf("ParseRange(%q) = %v-%v, want %s-%s", tt.in, r.Start, r.End, tt.start, tt.end)
		}
	}
}

func TestListContainsAndFirst(t *testing.T) {
	l, err := Parse([]string{"192.168.10.120", "192.168.10.100-192.168.10.110"})
	if err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]bool{
		"192.168.10.100": true,
		"192.168.10.105": true,
		"192.168.10.110": true,
		"192.168.10.111": false,
		"192.168.10.120": true,
		"192.168.10.99":  false,
	} {
		if got := l.Contains(netip.MustParseAddr(ip)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", ip, got, want)
		}
	}
	if first, ok := l.First(); !ok || first.String() != "192.168.10.120" {
		t.Errorf("First() = %v, %v, want 192.168.10.120", first, ok)
	}
	if _, ok := (List{}).First(); ok {
		t.Error("First() on empty list should return false")
	}
	if _, err := Parse([]string{"10.0.0.1", "bad"}); err == nil {
		t.Error("Parse should fail on invalid item")
	}
}
