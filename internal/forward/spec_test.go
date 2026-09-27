package forward

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in, canonical string
		want          Spec
	}{
		{"L:5432:db.internal:5432", "", Spec{Local, Endpoint{Port: 5432}, Endpoint{Host: "db.internal", Port: 5432}}},
		{"l:127.0.0.1:8080:localhost:80", "L:127.0.0.1:8080:localhost:80", Spec{Local, Endpoint{Host: "127.0.0.1", Port: 8080}, Endpoint{Host: "localhost", Port: 80}}},
		{"R:8080:localhost:3000", "", Spec{Remote, Endpoint{Port: 8080}, Endpoint{Host: "localhost", Port: 3000}}},
		{"R:0:localhost:3000", "", Spec{Remote, Endpoint{Port: 0}, Endpoint{Host: "localhost", Port: 3000}}},
		{"R:*:8080:localhost:3000", "", Spec{Remote, Endpoint{Host: "*", Port: 8080}, Endpoint{Host: "localhost", Port: 3000}}},
		{"D:1080", "", Spec{Dynamic, Endpoint{Port: 1080}, Endpoint{}}},
		{"D:[::1]:1080", "", Spec{Dynamic, Endpoint{Host: "::1", Port: 1080}, Endpoint{}}},
		{"L:[::1]:8080:[fd00::5]:80", "", Spec{Local, Endpoint{Host: "::1", Port: 8080}, Endpoint{Host: "fd00::5", Port: 80}}},
		{"L:2375:/var/run/docker.sock", "", Spec{Local, Endpoint{Port: 2375}, Endpoint{Socket: "/var/run/docker.sock"}}},
		{"L:localhost:2375:/var/run/docker.sock", "", Spec{Local, Endpoint{Host: "localhost", Port: 2375}, Endpoint{Socket: "/var/run/docker.sock"}}},
		{"L:/tmp/docker.sock:/var/run/docker.sock", "", Spec{Local, Endpoint{Socket: "/tmp/docker.sock"}, Endpoint{Socket: "/var/run/docker.sock"}}},
		{"L:/tmp/db.sock:db:5432", "", Spec{Local, Endpoint{Socket: "/tmp/db.sock"}, Endpoint{Host: "db", Port: 5432}}},
		{"R:/run/user/1000/gnupg/S.gpg-agent:/run/user/1000/gnupg/S.gpg-agent.extra", "", Spec{Remote, Endpoint{Socket: "/run/user/1000/gnupg/S.gpg-agent"}, Endpoint{Socket: "/run/user/1000/gnupg/S.gpg-agent.extra"}}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := Parse(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
			canonical := tt.canonical
			if canonical == "" {
				canonical = tt.in
			}
			if got.String() != canonical {
				t.Errorf("String() = %q, want %q", got.String(), canonical)
			}
			// Canonical form must round-trip.
			if again, err := Parse(got.String()); err != nil || again != got {
				t.Errorf("round trip: %+v, %v", again, err)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := map[string]string{
		"":                       "must start with",
		"5432:db:5432":           "must start with",
		"X:1:a:2":                "unknown kind",
		"L:5432":                 "missing target",
		"L:5432:db":              "missing target",
		"L:0:db:5432":            "invalid port",
		"L:70000:db:5432":        "invalid port",
		"L:5432:db:http":         "invalid port",
		"L:a:b:5432:db:5432":     "expected [bind:]port",
		"L:5432::5432":           "empty field",
		"L:5432:db:5432:":        "trailing ':'",
		"L:[::1:8080:db:80":      "unterminated",
		"L:[::1]x:8080:db:80":    "expected ':' after ']'",
		"D:/tmp/socks.sock":      "can't listen on a Unix socket",
		"D:0":                    "invalid port",
		"L:5432:d b:5432":        "invalid host",
		"L:/a.sock:x:/b.sock":    `invalid host "/a.sock"`,
		"R:8080:localhost:-1":    "invalid port",
		"L:bind:/x.sock:/y.sock": `invalid port "/x.sock"`,
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			_, err := Parse(in)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("got %v, want error containing %q", err, want)
			}
		})
	}
}
