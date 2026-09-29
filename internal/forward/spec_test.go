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
		{"L:0:db:5432", "", Spec{Local, Endpoint{Port: 0}, Endpoint{Host: "db", Port: 5432}}},
		{"D:0", "", Spec{Dynamic, Endpoint{Port: 0}, Endpoint{}}},
		{"R:1080", "", Spec{Remote, Endpoint{Port: 1080}, Endpoint{}}},
		{"R:127.0.0.1:1080", "", Spec{Remote, Endpoint{Host: "127.0.0.1", Port: 1080}, Endpoint{}}},
		{"h:8080", "H:8080", Spec{HTTP, Endpoint{Port: 8080}, Endpoint{}}},
		{"H:0.0.0.0:3128", "", Spec{HTTP, Endpoint{Host: "0.0.0.0", Port: 3128}, Endpoint{}}},
		{"K:8080:web/svc/frontend:80", "", Spec{Kube, Endpoint{Port: 8080}, Endpoint{Host: "web/svc/frontend", Port: 80}}},
		{"K:127.0.0.1:0:prod/web/service/frontend:80", "K:127.0.0.1:0:prod/web/svc/frontend:80", Spec{Kube, Endpoint{Host: "127.0.0.1", Port: 0}, Endpoint{Host: "prod/web/svc/frontend", Port: 80}}},
		{"K:8080:arn:aws:eks:us-east-1:1:cluster/prod/db/pod/pg-0:5432", "", Spec{Kube, Endpoint{Port: 8080}, Endpoint{Host: "arn:aws:eks:us-east-1:1:cluster/prod/db/pod/pg-0", Port: 5432}}},
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
		"":                        "must start with",
		"5432:db:5432":            "must start with",
		"X:1:a:2":                 "unknown kind",
		"L:5432":                  "missing target",
		"L:5432:db":               "missing target",
		"L:70000:db:5432":         "invalid port",
		"L:5432:db:http":          "invalid port",
		"L:a:b:5432:db:5432":      "expected [bind:]port",
		"L:5432::5432":            "empty field",
		"L:5432:db:5432:":         "trailing ':'",
		"L:[::1:8080:db:80":       "unterminated",
		"L:[::1]x:8080:db:80":     "expected ':' after ']'",
		"D:/tmp/socks.sock":       "can't listen on a Unix socket",
		"H:/tmp/http.sock":        "can't listen on a Unix socket",
		"R:5432:db":               "missing target",
		"K:8080:web/svc/frontend": "missing target port",
		"K:8080:svc/frontend:80":  "want [CONTEXT/]NAMESPACE/KIND/NAME",
		"K:8080:web/job/x:80":     "unknown kind",
		"K:8080:web/svc/Front:80": "invalid namespace or name",
		"K:x:web/svc/frontend:80": "expected [bind:]port",
		"K:8080:web/svc/f:0":      "invalid port",
		"L:5432:d b:5432":         "invalid host",
		"L:/a.sock:x:/b.sock":     `invalid host "/a.sock"`,
		"R:8080:localhost:-1":     "invalid port",
		"L:bind:/x.sock:/y.sock":  `invalid port "/x.sock"`,
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

func TestKinds(t *testing.T) {
	for in, want := range map[string]struct{ proxy, reverse, auto bool }{
		"L:5432:db:5432": {}, "L:0:db:5432": {auto: true}, "R:0:localhost:3000": {},
		"D:1080": {proxy: true}, "D:0": {proxy: true, auto: true}, "R:1080": {proxy: true, reverse: true},
		"H:8080": {proxy: true}, "K:0:web/svc/f:80": {auto: true},
	} {
		s, _ := Parse(in)
		if s.IsProxy() != want.proxy || s.IsReverseSOCKS() != want.reverse || s.AutoPort() != want.auto {
			t.Errorf("%s: proxy=%v reverse=%v auto=%v", in, s.IsProxy(), s.IsReverseSOCKS(), s.AutoPort())
		}
	}
	s, _ := Parse("K:8080:arn:x:cluster/prod/db/pod/pg-0:5432")
	if k := s.KubeRef(); k.Context != "arn:x:cluster/prod" || k.Namespace != "db" || k.Object() != "pod/pg-0" {
		t.Errorf("KubeRef = %+v", k)
	}
}

func TestInput(t *testing.T) {
	for in, want := range map[string]struct{ label, spec string }{
		"5432":                        {"", "L:5432:localhost:5432"},
		"8080:3000":                   {"", "L:8080:localhost:3000"},
		"db.internal:5432":            {"", "L:5432:db.internal:5432"},
		"8080:db.internal:5432":       {"", "L:8080:db.internal:5432"},
		"[fd00::5]:80":                {"", "L:80:[fd00::5]:80"},
		"socks":                       {"", "D:1080"},
		"RSOCKS":                      {"", "R:1080"},
		"http":                        {"", "H:8080"},
		"postgres=L:5432:db:5432":     {"postgres", "L:5432:db:5432"},
		" web = K:8080:web/svc/f:80 ": {"web", "K:8080:web/svc/f:80"},
		"pg=5432":                     {"pg", "L:5432:localhost:5432"},
		"0:db:5432":                   {"", "L:0:db:5432"},
		"8:db:80":                     {"", "L:8:db:80"},
	} {
		label, spec, err := ParseInput(in)
		if err != nil || label != want.label || spec.String() != want.spec {
			t.Errorf("ParseInput(%q) = %q, %s, %v; want %q, %s", in, label, spec, err, want.label, want.spec)
		}
	}
	for _, bad := range []string{"1pg=5432", "a b=5432", "nonsense", "=5432"} {
		if _, _, err := ParseInput(bad); err == nil {
			t.Errorf("ParseInput(%q) accepted", bad)
		}
	}
	if Expand("gpg-agent") != "gpg-agent" {
		t.Error("unknown names should pass through")
	}
}

func TestDescribe(t *testing.T) {
	for in, want := range map[string]string{
		"L:5432:db.internal:5432":         "localhost:5432 here → db.internal:5432, reached from devbox",
		"L:5432:localhost:5432":           "localhost:5432 here → port 5432 on devbox",
		"L:0.0.0.0:8080:localhost:80":     "port 8080 on all interfaces here → port 80 on devbox",
		"L:2375:/var/run/docker.sock":     "localhost:2375 here → /var/run/docker.sock on devbox",
		"R:8080:localhost:3000":           "localhost:8080 on devbox → port 3000 here",
		"R:9000:nas.lan:9000":             "localhost:9000 on devbox → nas.lan:9000, reached from this machine",
		"D:1080":                          "SOCKS proxy at localhost:1080 here, connecting out from devbox",
		"R:1080":                          "SOCKS proxy at localhost:1080 on devbox, connecting out from this machine",
		"H:0":                             "HTTP and SOCKS proxy at a free port here, connecting out from devbox",
		"K:8080:prod/web/svc/frontend:80": "localhost:8080 here → svc/frontend port 80 (namespace web, context prod), via kubectl on devbox",
	} {
		s, err := Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := Describe(s, "devbox"); got != want {
			t.Errorf("Describe(%s) =\n  %s\nwant\n  %s", in, got, want)
		}
	}
}
