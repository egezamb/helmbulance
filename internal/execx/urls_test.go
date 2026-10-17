package execx

import (
	"strings"
	"testing"
)

func TestRedactURLs(t *testing.T) {
	cases := map[string]string{
		"https://user:hunter2@charts.example.com/c-1.0.0.tgz":     "https://***@charts.example.com/c-1.0.0.tgz",
		"https://ghp_token@github.com/o/r":                        "https://***@github.com/o/r",
		"URL='http://hb-zd-web:80/health?token=abc&x=1'; while …": "URL='http://hb-zd-web:80/health?token=***&x=***'; while …",
		"http://svc:80/?flag#frag":                                "http://svc:80/?flag#frag",
		"oci://ghcr.io/o/charts/app --version 1.0.0":              "oci://ghcr.io/o/charts/app --version 1.0.0",
		"see http://a/b?q=1, then http://u:p@c/d":                 "see http://a/b?q=***, then http://***@c/d",
		"http://svc/p?a=1#frag":                                   "http://svc/p?a=***#frag",
		"no url here, just user@host and a:b":                     "no url here, just user@host and a:b",
		// A comma list of values files is one match; each URL's credentials must go (review item 1).
		"https://u:p1@a.example/v1.yaml,https://u:p2@b.example/v2.yaml": "https://***@a.example/v1.yaml,https://***@b.example/v2.yaml",
	}
	for in, want := range cases {
		if got := RedactURLs(in); got != want {
			t.Errorf("RedactURLs(%q) = %q, want %q", in, got, want)
		}
	}
}

// --from, a remote chart and the traffic pod's script all go into report.json's commands.
func TestMaskRedactsURLs(t *testing.T) {
	got := Mask("helm", []string{"install", "r", "https://u:hunter2@example.com/c.tgz"})
	if strings.Contains(got, "hunter2") {
		t.Errorf("Mask = %q, want the URL's credentials hidden", got)
	}
}

func TestRunnerRedactsURLsInErrors(t *testing.T) {
	_, stderr, err := (&Runner{}).RunCapture(t.Context(), "/bin/sh", "-c",
		`echo "Error: failed to fetch https://u:hunter2@example.com/c.tgz : 401" >&2; exit 1`, "https://u:hunter2@example.com/c.tgz")
	if err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(string(stderr), "hunter2") {
		t.Errorf("err = %v, stderr = %q, want the credentials hidden", err, stderr)
	}
}
