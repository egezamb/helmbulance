package scenario

import (
	"errors"
	"strings"
	"testing"

	"github.com/egezamb/helmbulance/internal/execx"
)

var (
	installCmd = []execx.Command{{Line: "helm install hb ./chart"}}
	reuseCmd   = []execx.Command{{Line: "helm upgrade hb ./chart"}}
)

// The control already showed the restart; a treatment that could not run must not lose it.
func TestRestartsSurviveAFailedTreatment(t *testing.T) {
	res := inconclusive(Result{}, "the upgrade with the values files failed", errors.New("timed out"))
	res = withRestarts(res, restarts{owners: []string{"Deployment/web"}, again: "no"}, installCmd, reuseCmd)
	if res.Verdict != Fail || len(res.Findings) != 1 || res.Findings[0].ID != "HB-UV-003" {
		t.Fatalf("verdict = %s with %d finding(s), want FAIL with HB-UV-003", res.Verdict, len(res.Findings))
	}
	if !strings.Contains(res.Summary, "the upgrade with the values files failed") {
		t.Errorf("summary %q lost why the treatment did not run", res.Summary)
	}
}

func TestRestartsTitleFollowsTheSecondUpgrade(t *testing.T) {
	cases := map[string]string{
		"yes":     "Every upgrade, even one that changes nothing, restarts pods",
		"no":      "The first upgrade after install restarts pods, even when it changes nothing",
		"unknown": "An upgrade that changes nothing restarts pods",
	}
	for again, want := range cases {
		res := withRestarts(Result{Verdict: Pass}, restarts{owners: []string{"Deployment/web"}, again: again}, installCmd, reuseCmd)
		if len(res.Findings) != 1 || res.Findings[0].Title != want {
			t.Errorf("again=%s: findings = %+v, want the title %q", again, res.Findings, want)
		}
		if again != "yes" && strings.HasPrefix(res.Findings[0].Impact, "Every") {
			t.Errorf("again=%s: impact %q claims every upgrade", again, res.Findings[0].Impact)
		}
	}
}

func TestNoRestartsNoFinding(t *testing.T) {
	res := withRestarts(Result{Verdict: Pass}, restarts{}, installCmd, reuseCmd)
	if res.Verdict != Pass || len(res.Findings) != 0 {
		t.Errorf("verdict = %s with %d finding(s), want PASS and none", res.Verdict, len(res.Findings))
	}
}
