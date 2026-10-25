package integrations

import "testing"

func TestBaseURLIsAnHTTPSOrigin(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://api.bugfix.es": true, "https://bugs.example.com/": true,
		"http://api.bugfix.es":                      false, // https only
		"http://169.254.169.254/latest/meta-data/?": false,
		"https://evil.example/?":                    false, // a query swallows the paths we append
		"https://evil.example/x?y=1":                false,
		"https://evil.example/#":                    false,
		"https://user:pass@evil.example":            false,
		"https://evil.example/latest/meta-data":     false, // a path would prefix ours
		"ftp://api.bugfix.es":                       false,
		"https://":                                  false,
	} {
		if got := validBaseURL(raw); got != ok {
			t.Errorf("%q valid = %v, want %v", raw, got, ok)
		}
	}
}
