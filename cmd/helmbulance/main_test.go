package main

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// stderrOf runs f and returns what it wrote to os.Stderr.
func stderrOf(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	f()
	os.Stderr = old
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

func TestRefuseRemote(t *testing.T) {
	cases := []struct {
		allow   bool
		refs    []string
		refused bool
		says    string
	}{
		{false, []string{"../../testdata/charts/keeps-secret", ""}, false, ""},
		{false, []string{"oci://x/y"}, true, "pass --allow-remote"},
		{true, []string{"oci://x/y"}, false, "warning: running oci://x/y"},
		{false, []string{"./nonexistent"}, true, "path not found"},
		{true, []string{"~/chart"}, true, "path not found"},
		{false, []string{"../../testdata/charts/keeps-secret", "bitnami/redis"}, true, "bitnami/redis is not a chart on disk"},
	}
	for _, c := range cases {
		var refused bool
		out := stderrOf(t, func() { refused = refuseRemote(c.allow, c.refs...) })
		if refused != c.refused || !strings.Contains(out, c.says) {
			t.Errorf("refuseRemote(%v, %q) = %v, %q; want %v and a message with %q", c.allow, c.refs, refused, out, c.refused, c.says)
		}
		if c.says == "" && out != "" {
			t.Errorf("refuseRemote(%v, %q) printed %q, want nothing", c.allow, c.refs, out)
		}
	}
}

// version1 stands in for helm show chart: every chart is version 1.0.0.
func version1(string) (string, error) { return "1.0.0", nil }

func TestRefuseFromVersionWithoutFrom(t *testing.T) {
	const local = "../../testdata/charts/upgrade-v2-ok"
	cases := []struct {
		chart, from, fromVersion string
		refused                  bool
	}{
		{local, "", "", false},
		{local, "", "1.0.0", true},
		{local, "../../testdata/charts/upgrade-v1", "1.0.0", false},
		{"repo/chart", "", "1.0.0", false},
	}
	for _, c := range cases {
		var refused bool
		out := stderrOf(t, func() { refused = refuseFromVersion(c.chart, c.from, c.fromVersion, version1) })
		if refused != c.refused {
			t.Errorf("refuseFromVersion(%q, %q, %q) = %v, want %v", c.chart, c.from, c.fromVersion, refused, c.refused)
		}
		if refused && !strings.Contains(out, "needs --from") {
			t.Errorf("message %q does not say --from is needed", out)
		}
	}
}

func TestRefuseFromVersionThatALocalFromIsNot(t *testing.T) {
	const v1 = "../../testdata/charts/upgrade-v1"
	var refused bool
	out := stderrOf(t, func() { refused = refuseFromVersion("./chart", v1, "1.2.3", version1) })
	if !refused || !strings.Contains(out, "is version 1.0.0, not --from-version 1.2.3") {
		t.Errorf("refused = %v, %q; want a refusal naming both versions", refused, out)
	}
	out = stderrOf(t, func() {
		refused = refuseFromVersion("./chart", v1, "1.0.0", func(string) (string, error) { return "", errors.New("boom") })
	})
	if !refused || !strings.Contains(out, "could not read the version") {
		t.Errorf("refused = %v, %q; want a refusal when the version cannot be read", refused, out)
	}
	if refuseFromVersion("./chart", v1, "v1.0.0", version1) {
		t.Error("--from-version v1.0.0 was refused for a chart at 1.0.0; helm treats them as the same")
	}
	if refuseFromVersion("./chart", "repo/chart", "1.2.3", version1) {
		t.Error("a remote --from was refused; helm honours --version there")
	}
}

func TestCheckRotations(t *testing.T) {
	for _, ok := range []string{"*-webhook-tls/*", "hb-upgrade-ca/tls.crt", "*/ca.crt"} {
		if err := checkRotations([]string{ok}); err != nil {
			t.Errorf("checkRotations(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"webhook-tls", "/tls.crt", "ca/", "ns/secret/key", "[ca/tls.crt"} {
		if err := checkRotations([]string{bad}); err == nil {
			t.Errorf("checkRotations(%q) = nil, want an error", bad)
		}
	}
}
