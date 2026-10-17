package scenario

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egezamb/helmbulance/internal/execx"
	"github.com/egezamb/helmbulance/internal/helm"
	"github.com/egezamb/helmbulance/internal/kube"
	"github.com/egezamb/helmbulance/internal/oracle"
)

// fakeUpgradeTools answers upgrade-without-values. Pod reads, in order, show pod-template-hash
// revisions[0], revisions[1], …, then the last one. The n-th Secret read fails when n is in
// failSecretRead; the second no-op upgrade or the treatment fail when told to.
const fakeUpgradeTools = `#!/bin/sh
count() { n=$(cat "$FAKE_DIR/$1" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$FAKE_DIR/$1"; echo $n; }
case "$(basename "$0") $*" in
  "helm upgrade"*"--reset-values"*) [ -n "$FAIL_TREATMENT" ] && { echo "Error: treatment failed" >&2; exit 1; }; exit 0 ;;
  "helm upgrade"*) n=$(count upgrade); [ "$n" = 2 ] && [ -n "$FAIL_SECOND_UPGRADE" ] && { echo "Error: busy" >&2; exit 1; }; exit 0 ;;
  "kubectl"*"get secrets"*) n=$(count secrets); case " $FAIL_SECRET_READ " in *" $n "*) echo "Error: forbidden" >&2; exit 1 ;; esac
    echo '{"items":[{"metadata":{"name":"s"},"type":"Opaque","data":{"k":"dg=="}}]}' ;;
  "kubectl"*"get pods"*) n=$(count pods); set -- $REVISIONS; i=1; rev=""
    for r in "$@"; do rev=$r; [ $i -ge $n ] && break; i=$((i+1)); done
    echo '{"items":[{"metadata":{"name":"web-'$rev'","uid":"'$rev'","labels":{"pod-template-hash":"'$rev'"},"ownerReferences":[{"kind":"ReplicaSet","name":"web-'$rev'"}]},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}]}' ;;
  "kubectl"*) echo '{"items":[]}' ;;
esac
exit 0
`

func runUpgradeValues(t *testing.T, env map[string]string) Result {
	t.Helper()
	bin, dir := t.TempDir(), t.TempDir()
	for _, name := range []string{"helm", "kubectl"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(fakeUpgradeTools), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DIR", dir)
	for _, k := range []string{"REVISIONS", "FAIL_TREATMENT", "FAIL_SECOND_UPGRADE", "FAIL_SECRET_READ"} {
		t.Setenv(k, env[k])
	}
	hasher, err := oracle.NewHasher()
	if err != nil {
		t.Fatal(err)
	}
	runner := &execx.Runner{}
	return UpgradeValues{}.Run(t.Context(), &Env{
		Runner: runner,
		Helm:   &helm.Client{Runner: runner, Timeout: time.Minute},
		Kube:   &kube.Client{Runner: runner},
		Hasher: hasher,
		Chart:  "./chart",
		Values: helm.Values{Files: []string{"values.yaml"}},
		Log:    io.Discard,
	})
}

// HB-UV-003 is found in the control, so it stands whatever happens to the treatment: the verdict is
// FAIL and the summary says what did not run.
func TestUpgradeValuesRestarts(t *testing.T) {
	cases := []struct {
		name      string
		env       map[string]string
		verdict   string
		title     string
		inSummary string
	}{
		{"every upgrade", map[string]string{"REVISIONS": "a b c"}, Fail,
			"Every upgrade, even one that changes nothing, restarts pods", ""},
		{"first upgrade only", map[string]string{"REVISIONS": "a b b"}, Fail,
			"The first upgrade after install restarts pods, even when it changes nothing", ""},
		{"second upgrade failed", map[string]string{"REVISIONS": "a b", "FAIL_SECOND_UPGRADE": "1"}, Fail,
			"An upgrade that changes nothing restarts pods", ""},
		{"treatment failed", map[string]string{"REVISIONS": "a b b", "FAIL_TREATMENT": "1"}, Fail,
			"The first upgrade after install restarts pods, even when it changes nothing", "the upgrade with the values files failed"},
		// The settled reader reads Secrets twice per snapshot (install 1-2, control 3-4), so read 5 is
		// the first one after the treatment.
		{"Secret read after the treatment failed", map[string]string{"REVISIONS": "a b b", "FAIL_SECRET_READ": "5"}, Fail,
			"The first upgrade after install restarts pods, even when it changes nothing", "could not read Secrets after the upgrade"},
		{"no restart", map[string]string{"REVISIONS": "a"}, Pass, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := runUpgradeValues(t, c.env)
			var titles []string
			for _, f := range res.Findings {
				if f.ID == "HB-UV-003" {
					titles = append(titles, f.Title)
				}
			}
			if res.Verdict != c.verdict || strings.Join(titles, "|") != c.title || !strings.Contains(res.Summary, c.inSummary) {
				t.Errorf("verdict %s, HB-UV-003 %q, summary %q; want %s, %q, summary with %q",
					res.Verdict, titles, res.Summary, c.verdict, c.title, c.inSummary)
			}
		})
	}
}
