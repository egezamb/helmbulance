package kube

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/egezamb/helmbulance/internal/execx"
)

func TestFinishedJob(t *testing.T) {
	for _, c := range []struct {
		pod  Pod
		want bool
	}{
		{Pod{Owner: "Job/migrate", Phase: "Failed"}, true},
		{Pod{Owner: "Job/migrate", Phase: "Succeeded"}, true},
		{Pod{Owner: "Job/migrate", Phase: "Pending"}, false},  // stuck in ErrImagePull
		{Pod{Owner: "Job/migrate", Phase: "Running"}, false},  // crash-looping in place
		{Pod{Owner: "StatefulSet/db", Phase: "Failed"}, false}, // not a Job
	} {
		if got := c.pod.FinishedJob(); got != c.want {
			t.Errorf("FinishedJob(%s, %s) = %v, want %v", c.pod.Owner, c.pod.Phase, got, c.want)
		}
	}
}

// kubectlPrinting puts a kubectl that prints out first on PATH.
func kubectlPrinting(t *testing.T, out string) *Client {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "out"), []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncat " + filepath.Join(bin, "out") + "\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &Client{Runner: &execx.Runner{}}
}

func TestPodsReadPhaseAndRevision(t *testing.T) {
	c := kubectlPrinting(t, `{"items":[
		{"metadata":{"name":"web-1","uid":"a","labels":{"pod-template-hash":"7d9f8c6b5"},"ownerReferences":[{"kind":"ReplicaSet","name":"web-7d9f8c6b5","uid":"rs-1"}]},
		 "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}},
		{"metadata":{"name":"db-0","uid":"b","labels":{"controller-revision-hash":"db-5f6c"},"ownerReferences":[{"kind":"StatefulSet","name":"db"}]},
		 "status":{"phase":"Running","containerStatuses":[{"restartCount":2}]}},
		{"metadata":{"name":"migrate-x","uid":"c","ownerReferences":[{"kind":"Job","name":"migrate"}]},"status":{"phase":"Failed"}},
		{"metadata":{"name":"migrate-y","uid":"d","ownerReferences":[{"kind":"Job","name":"migrate"}]},"status":{"phase":"Succeeded"}}
	]}`)
	got, err := c.Pods(t.Context(), "ns")
	if err != nil {
		t.Fatal(err)
	}
	want := []Pod{
		{Name: "web-1", UID: "a", Owner: "ReplicaSet/web-7d9f8c6b5", OwnerUID: "rs-1", Ready: true, Phase: "Running", Revision: "7d9f8c6b5"},
		{Name: "db-0", UID: "b", Owner: "StatefulSet/db", Restarts: 2, Phase: "Running", Revision: "db-5f6c"},
		{Name: "migrate-x", UID: "c", Owner: "Job/migrate", Phase: "Failed"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Pods =\n%+v\nwant\n%+v", got, want)
	}
}

func TestFailedJobs(t *testing.T) {
	c := kubectlPrinting(t, `{"items":[
		{"metadata":{"name":"seed"},"status":{"conditions":[{"type":"Complete","status":"True"}]}},
		{"metadata":{"name":"migrate"},"status":{"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded","message":"Job has reached the specified backoff limit"}]}},
		{"metadata":{"name":"running"},"status":{}}
	]}`)
	got, err := c.FailedJobs(t.Context(), "ns")
	want := []string{"Job/migrate: BackoffLimitExceeded Job has reached the specified backoff limit"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("FailedJobs = %q, %v; want %q", got, err, want)
	}
}

// With the default backoffLimit, a Job gets its Failed condition only after about ten minutes of
// back-off, so a Job that fails every attempt must count while it still retries. One that
// succeeded after a failed attempt is fine.
func TestFailedJobsCountsJobsStillRetrying(t *testing.T) {
	c := kubectlPrinting(t, `{"items":[
		{"metadata":{"name":"migrate"},"status":{"failed":2,"active":1}},
		{"metadata":{"name":"flaky"},"status":{"failed":1,"succeeded":1,"conditions":[{"type":"Complete","status":"True"}]}},
		{"metadata":{"name":"gaveup"},"status":{"failed":7,"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded"}]}},
		{"metadata":{"name":"fine"},"status":{"active":1,"conditions":[{"type":"Failed","status":"False"}]}}
	]}`)
	got, err := c.FailedJobs(t.Context(), "ns")
	want := []string{"Job/gaveup: BackoffLimitExceeded", "Job/migrate: 2 failed attempt(s), still retrying"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("FailedJobs = %q, %v; want %q", got, err, want)
	}
}
