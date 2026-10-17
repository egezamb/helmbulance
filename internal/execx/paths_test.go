package execx

import (
	"strings"
	"testing"
)

// A killed helm has no "Error:" line, so the whole error is kept, with the command line. Its
// --kubeconfig path holds the user name, and the transcript goes into report.json.
func TestRunnerHidesLocalPaths(t *testing.T) {
	r := &Runner{Paths: map[string]string{"/home/ege": "~", "/srv/hb-state": "$HELMBULANCE_STATE_DIR"}}
	_, stderr, err := r.RunCapture(t.Context(), "/bin/sh", "-c",
		`echo "open /srv/hb-state: denied; cache /home/ege/.cache, other /home/eger/x" >&2; exit 1`,
		"--kubeconfig", "/home/ege/.helmbulance/clusters/c/kubeconfig")
	want := []string{
		"--kubeconfig ~/.helmbulance/clusters/c/kubeconfig",
		"open $HELMBULANCE_STATE_DIR: denied",
		"cache ~/.cache,",
		"other /home/eger/x", // another path that only starts with $HOME stays
	}
	for _, text := range []string{err.Error(), string(stderr), r.Transcript()[0].Line} {
		if strings.Contains(text, "/home/ege/") || strings.Contains(text, "/srv/hb-state") {
			t.Errorf("local path left in %q", text)
		}
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) && !strings.Contains(string(stderr), w) {
			t.Errorf("want %q in the error, got: %v", w, err)
		}
	}
}
