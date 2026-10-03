package openssh

import "testing"

func TestParseAddress(t *testing.T) {
	for _, c := range []struct{ out, want string }{
		{"user me\nhostname 192.168.122.10\nport 22\n", "192.168.122.10:22"},
		{"hostname fe80::1\nport 2222\nproxycommand none\n", "[fe80::1]:2222"},
		{"hostname dev\nport 22\nproxyjump bastion\n", ""},
		{"hostname dev\nport 22\nproxycommand nc %h %p\n", ""},
		{"user me\n", ""},
	} {
		if got := parseAddress([]byte(c.out)); got != c.want {
			t.Errorf("parseAddress(%q) = %q, want %q", c.out, got, c.want)
		}
	}
}
