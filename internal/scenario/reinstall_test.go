package scenario

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/egezamb/helmbulance/internal/oracle"
)

func TestReinstallFindings(t *testing.T) {
	consumers := map[string][]string{"hb-reinstall-db/*": {"StatefulSet/hb-reinstall-db"}}
	stateful := map[string]bool{"StatefulSet/hb-reinstall-db": true}
	pw := []oracle.Change{{Ref: "hb-reinstall-db/password", Kind: oracle.Changed}}
	kept := []volume{{claim: "data-hb-reinstall-db-0", state: "kept"}}
	users := map[string][]string{"data-hb-reinstall-db-0": {"StatefulSet/hb-reinstall-db"}}

	got, _, _ := reinstallFindings(kept, pw, consumers, stateful, true, users, nil)
	if len(got) != 1 || got[0].ID != "HB-RI-001" || got[0].Severity != "high" {
		t.Fatalf("kept data and a new password: findings = %+v, want one high HB-RI-001", got)
	}
	if ev := strings.Join(got[0].Evidence, " "); !strings.Contains(ev, "data-hb-reinstall-db-0") || !strings.Contains(ev, "password` changed") {
		t.Errorf("evidence %q does not name the volume and the key", ev)
	}

	// New credentials with no data left behind are a fresh install: nothing to report.
	deleted := []volume{{claim: "data", state: "deleted"}}
	if got, _, _ := reinstallFindings(deleted, pw, consumers, stateful, true, users, nil); len(got) != 0 {
		t.Errorf("deleted volume and a new password: findings = %+v, want none", got)
	}
	// Kept data and kept credentials: nothing to report.
	if got, _, _ := reinstallFindings(kept, nil, consumers, stateful, true, users, nil); len(got) != 0 {
		t.Errorf("kept data and kept Secrets: findings = %+v, want none", got)
	}
	// A kept claim whose sentinel is gone.
	emptied := []volume{{claim: "data", state: "missing"}, {claim: "logs", state: "replaced"}}
	if got, _, _ := reinstallFindings(emptied, nil, consumers, stateful, true, users, nil); len(got) != 1 || got[0].ID != "HB-RI-002" {
		t.Errorf("emptied volumes: findings = %+v, want HB-RI-002", got)
	}
}

// A key that changed but is read only by a workload without a kept volume is not tied to the data.
func TestReinstallFindingsIgnoreUnrelatedKeys(t *testing.T) {
	consumers := map[string][]string{"webhook-tls/*": {"Deployment/webhook"}}
	kept := []volume{{claim: "data-redis-0", state: "kept"}}
	users := map[string][]string{"data-redis-0": {"StatefulSet/redis"}}
	cert := []oracle.Change{{Ref: "webhook-tls/tls.crt", Kind: oracle.Changed}}
	got, unrelated, _ := reinstallFindings(kept, cert, consumers, nil, true, users, nil)
	if len(got) != 0 || len(unrelated) != 1 {
		t.Errorf("findings = %+v, unrelated = %v; want no finding and the certificate set apart", got, unrelated)
	}
}

// A certificate the kept volume's own workload reads is a reissue, not a credential mismatch: no
// finding, and it is set apart as a certificate rather than reported (review item 3).
func TestReinstallFindingsCertReadByKeeper(t *testing.T) {
	consumers := map[string][]string{"db-tls/*": {"StatefulSet/db"}}
	kept := []volume{{claim: "data-db-0", state: "kept"}}
	users := map[string][]string{"data-db-0": {"StatefulSet/db"}}
	stateful := map[string]bool{"StatefulSet/db": true}
	cert := []oracle.Change{{Ref: "db-tls/tls.crt", Kind: oracle.Changed}}
	got, unrelated, certs := reinstallFindings(kept, cert, consumers, stateful, true, users, nil)
	if len(got) != 0 {
		t.Errorf("findings = %+v, want none for a reissued certificate a keeper reads", got)
	}
	if len(certs) != 1 || certs[0].Ref != "db-tls/tls.crt" {
		t.Errorf("certs = %v, want the certificate set apart", certs)
	}
	if len(unrelated) != 0 {
		t.Errorf("unrelated = %v, want empty since the keeper does read it", unrelated)
	}
}

// Only a --wait timeout with the release applied is worth collecting evidence for; a cancel, the
// chart's webhook, a pull error or a render refusal are not (review item 1).
func TestTimedOutWaiting(t *testing.T) {
	for _, c := range []struct {
		err  string
		want bool
	}{
		{"Error: INSTALLATION FAILED: timed out waiting for the condition", true},
		{"Error: INSTALLATION FAILED: context deadline exceeded", true},
		{`Error: INSTALLATION FAILED: failed calling webhook "v.io": context deadline exceeded`, false},
		{"Failed to pull image: toomanyrequests: rate limit", false},
		{"Error: INSTALLATION FAILED: execution error at (demo/templates/secret.yaml:3:4)", false},
	} {
		if got := timedOutWaiting(t.Context(), errors.New(c.err)); got != c.want {
			t.Errorf("timedOutWaiting(%q) = %v, want %v", c.err, got, c.want)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if timedOutWaiting(ctx, errors.New("timed out waiting for the condition")) {
		t.Error("a cancelled run should not be treated as a collectable timeout")
	}
}

func TestReinstallPassSummary(t *testing.T) {
	for _, c := range []struct {
		volumes []volume
		changes []oracle.Change
		want    string
	}{
		{nil, nil, "Secrets came back unchanged after uninstall and install; the release has no volumes"},
		{nil, []oracle.Change{{Ref: "s/k", Kind: oracle.Changed}}, "the release has no volumes, so new Secrets after a reinstall lose nothing"},
		{[]volume{{claim: "a", state: "kept"}}, nil, "Secrets came back unchanged after uninstall and install; 1 volume(s) kept their data"},
		{[]volume{{claim: "a", state: "recreated"}}, []oracle.Change{{Ref: "s/k", Kind: oracle.Changed}},
			"no volume kept its data, so new Secrets are expected; deleted with the release (depends on the StorageClass reclaim policy): `a`"},
	} {
		if got := reinstallPassSummary(c.volumes, c.changes); got != c.want {
			t.Errorf("reinstallPassSummary = %q, want %q", got, c.want)
		}
	}
}

func TestReinstallInstallFailed(t *testing.T) {
	for _, c := range []struct {
		err, verdict string
	}{
		{"Error: INSTALLATION FAILED: execution error at (demo/templates/secret.yaml:3:4): set auth.existingSecret to reinstall over kept data", Pass},
		{"Error: INSTALLATION FAILED: context deadline exceeded", Inconclusive},
		{"helm install hb-reinstall ./chart: signal: killed", Inconclusive},
		{"Failed to pull image: toomanyrequests: rate limit", Inconclusive},
		{`Error: INSTALLATION FAILED: Internal error occurred: failed calling webhook "v.example.io": context deadline exceeded`, Fail},
		{`Error: INSTALLATION FAILED: Secret "hb-reinstall-db" exists and cannot be imported into the current release: invalid ownership metadata`, Fail},
	} {
		res := Reinstall{}.installFailed(t.Context(), Result{}, errors.New(c.err), nil)
		if res.Verdict != c.verdict {
			t.Errorf("%q: verdict = %s (%s), want %s", c.err, res.Verdict, res.Summary, c.verdict)
		}
		if c.verdict == Fail && (len(res.Findings) != 1 || res.Findings[0].ID != "HB-RI-003") {
			t.Errorf("%q: findings = %+v, want HB-RI-003", c.err, res.Findings)
		}
	}
}

func TestSentinelRepro(t *testing.T) {
	got := strings.Join(sentinelRepro("write", "echo marker > /pvc/marker"), "\n")
	for _, want := range []string{"# write", "kubectl -n hb-reinstall run marker", `"claimName":"CLAIM"`, "echo marker > /pvc/marker"} {
		if !strings.Contains(got, want) {
			t.Errorf("sentinelRepro does not contain %q:\n%s", want, got)
		}
	}
}
