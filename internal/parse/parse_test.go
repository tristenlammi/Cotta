package parse

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in         string
		user, host string
		port       int
		name       string
	}{
		{"ssh user@host -p 2222", "user", "host", 2222, "host"},
		{"ssh -p 2222 deploy@web.example.com", "deploy", "web.example.com", 2222, "web"},
		{"ssh -p2222 deploy@web.example.com", "deploy", "web.example.com", 2222, "web"},
		{"$ ssh -vA -i ~/.ssh/id_ed25519 -l root 10.0.0.1", "root", "10.0.0.1", 22, "10.0.0.1"},
		{"ssh -o Port=2200 -o User=me box.local uptime -p 9", "me", "box.local", 2200, "box"},
		{"ssh -l flag other@host", "flag", "host", 22, "host"},
		{"user@host", "user", "host", 22, "host"},
		{"user@host:2222", "user", "host", 2222, "host"},
		{"192.168.1.20", "", "192.168.1.20", 22, "192.168.1.20"},
		{"192.168.1.20:2022", "", "192.168.1.20", 2022, "192.168.1.20"},
		{"ssh://admin@router.lan:8022/", "admin", "router.lan", 8022, "router"},
		{"root@[2001:db8::1]:2222", "root", "2001:db8::1", 2222, "2001:db8::1"},
		{"2001:db8::1", "", "2001:db8::1", 22, "2001:db8::1"},
		{"sftp -P 2022 me@files.example.com", "me", "files.example.com", 2022, "files"},
		{"  pi@raspberrypi.local  ", "pi", "raspberrypi.local", 22, "raspberrypi"},
	}
	for _, c := range cases {
		r, err := Parse(c.in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.in, err)
			continue
		}
		if r.User != c.user || r.Host != c.host || r.Port != c.port || r.Name != c.name {
			t.Errorf("%q: got %s@%s:%d (%s), want %s@%s:%d (%s)", c.in, r.User, r.Host, r.Port, r.Name, c.user, c.host, c.port, c.name)
		}
	}
}

func TestParseIdentityAndWarnings(t *testing.T) {
	r, err := Parse("ssh -i ~/.ssh/work -J bastion me@inner")
	if err != nil {
		t.Fatal(err)
	}
	if r.IdentityFile != "~/.ssh/work" || len(r.Warnings) != 1 || r.Host != "inner" {
		t.Errorf("got %+v", r)
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{"", "   ", "ssh", "ssh -p 22", "hello world", "user@", "host:99999", "ssh -p abc host", "bad host!", "[::1"} {
		if r, err := Parse(in); err == nil {
			t.Errorf("%q: expected an error, got %+v", in, r)
		}
	}
}
