package scenario

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/egezamb/helmbulance/internal/execx"
	"github.com/egezamb/helmbulance/internal/kube"
)

// fakePodReads puts a kubectl on PATH whose n-th "get pods" prints reads[n] (then the last one),
// with no Jobs, and returns an Env that uses it.
func fakePodReads(t *testing.T, reads ...string) *Env {
	t.Helper()
	bin, dir := t.TempDir(), t.TempDir()
	for i, r := range reads {
		if err := os.WriteFile(filepath.Join(dir, "pods."+strconv.Itoa(i)), []byte(r), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
case "$*" in
  *"get jobs"*) if [ -n "$FAKE_JOBS" ]; then echo "$FAKE_JOBS"; else echo '{"items":[]}'; fi ;;
  *"get pods"*)
    n=$(cat "$FAKE_DIR/n" 2>/dev/null || echo 0)
    f="$FAKE_DIR/pods.$n"; [ -f "$f" ] || f="$FAKE_DIR/pods.` + strconv.Itoa(len(reads)-1) + `"
    cat "$f"; echo $((n+1)) > "$FAKE_DIR/n" ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DIR", dir)
	runner := &execx.Runner{}
	return &Env{Runner: runner, Kube: &kube.Client{Runner: runner}, Log: io.Discard}
}

func podsJSON(uid string, restarts int) string {
	return `{"items":[{"metadata":{"name":"web-` + uid + `","uid":"` + uid + `","ownerReferences":[{"kind":"ReplicaSet","name":"web-1"}]},` +
		`"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"restartCount":` + strconv.Itoa(restarts) + `}]}}]}`
}

func TestUnhealthyAfterRestartBaseline(t *testing.T) {
	cases := []struct {
		name     string
		before   []kube.Pod
		upgraded []kube.Pod
		reads    []string
		want     string
	}{
		// Row F: a new pod that restarted once during its own startup, before helm --wait returned.
		{"new pod, restart before the first read", nil, nil, []string{podsJSON("new", 1)}, ""},
		// Row D: a new pod that crashes after helm --wait returned.
		{"new pod, restart while watched", nil, nil, []string{podsJSON("new", 0), podsJSON("new", 2)}, "restarted 2 more time(s)"},
		// A new pod read right after the upgrade, which crashes while the Secrets settle, before
		// the first Settle read.
		{"new pod, restart before Settle but after the upgrade", nil, []kube.Pod{{UID: "new", Restarts: 0}}, []string{podsJSON("new", 1)}, "restarted 1 more time(s)"},
		// A pod that survived the upgrade keeps its count from before the upgrade.
		{"surviving pod", []kube.Pod{{UID: "old", Restarts: 0}}, nil, []string{podsJSON("old", 1)}, "restarted 1 more time(s)"},
		// The read after the upgrade does not reset a pod that survived it.
		{"surviving pod, read again after the upgrade", []kube.Pod{{UID: "old", Restarts: 0}}, []kube.Pod{{UID: "old", Restarts: 1}}, []string{podsJSON("old", 1)}, "restarted 1 more time(s)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			shortSettle(t, 2*time.Second, 50*time.Millisecond)
			env := fakePodReads(t, c.reads...)
			got := strings.Join(unhealthyAfter(t.Context(), env, c.before, c.upgraded, nil), "; ")
			if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
				t.Errorf("unhealthyAfter = %q, want %q", got, c.want)
			}
		})
	}
}

// A Job that had already failed before the upgrade is the earlier version's, not this upgrade's.
func TestUnhealthyAfterSkipsJobsThatFailedBefore(t *testing.T) {
	shortSettle(t, 2*time.Second, 50*time.Millisecond)
	env := fakePodReads(t, podsJSON("p", 0))
	t.Setenv("FAKE_JOBS", `{"items":[{"metadata":{"name":"old"},"status":{"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded"}]}}]}`)
	before := []kube.Pod{{UID: "p"}}
	if got := unhealthyAfter(t.Context(), env, before, nil, []string{"Job/old: BackoffLimitExceeded"}); len(got) != 0 {
		t.Errorf("unhealthyAfter = %q, want nothing for a Job that failed before", got)
	}
	if got := unhealthyAfter(t.Context(), env, before, nil, nil); len(got) != 1 {
		t.Errorf("unhealthyAfter = %q, want the Job when it is new", got)
	}
}
