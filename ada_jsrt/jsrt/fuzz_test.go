package jsrt

import "testing"

func FuzzPathPattern(f *testing.F) {
	f.Add("/workspace/**", "/workspace/a/b.txt")
	f.Add("/workspace/*.txt", "/workspace/a.txt")
	f.Add("/workspace/**/*.json", "/workspace/x/y.json")
	f.Fuzz(func(t *testing.T, pattern, value string) {
		p, err := compilePathPattern(pattern, "/workspace", pathCaseFold())
		if err != nil {
			return
		}
		_ = p.match(value)
	})
}

func FuzzNetRule(f *testing.F) {
	f.Add("http://example.com:8080", "example.com", 8080, "http")
	f.Add("*.example.com", "api.example.com", 443, "https")
	f.Add("10.0.0.0/8", "10.1.2.3", 80, "http")
	f.Fuzz(func(t *testing.T, raw, host string, port int, scheme string) {
		r, err := parseNetRule(raw)
		if err != nil {
			return
		}
		_ = r.match(host, port, scheme)
	})
}
