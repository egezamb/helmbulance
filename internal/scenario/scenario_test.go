package scenario

import (
	"reflect"
	"testing"

	"github.com/egezamb/helmbulance/internal/kube"
)

func TestReplacedPods(t *testing.T) {
	before := []kube.Pod{
		{UID: "a", Owner: "ReplicaSet/web-7d9f8c6b5", Revision: "7d9f8c6b5"},
		{UID: "b", Owner: "StatefulSet/db", Revision: "db-5f6c"},
		{UID: "c", Owner: "ReplicaSet/worker-55c4d", Revision: "55c4d"},
	}
	after := []kube.Pod{
		{UID: "a2", Owner: "ReplicaSet/web-6c8b7d4f9", Revision: "6c8b7d4f9"}, // web was rolled
		{UID: "b", Owner: "StatefulSet/db", Revision: "db-5f6c"},              // db kept its pod
		{UID: "c", Owner: "ReplicaSet/worker-55c4d", Revision: "55c4d"},
	}
	want := []string{"Deployment/web"}
	if got := replacedPods(before, after); !reflect.DeepEqual(got, want) {
		t.Errorf("replacedPods = %v, want %v", got, want)
	}
}

// Pods that go away without a new revision are not a restart: issue #9 found each of these
// reported as HB-UV-003.
func TestReplacedPodsIgnoresPodsThatLeaveWithoutARollout(t *testing.T) {
	before := []kube.Pod{
		{UID: "j", Owner: "Job/seed", Phase: "Running"},                       // a non-hook Job that then finishes
		{UID: "h", Owner: "Job/hook-migrate", Phase: "Failed"},                // a failed hook attempt, deleted by before-hook-creation
		{UID: "w1", Owner: "ReplicaSet/web-7d9f8c6b5", Revision: "7d9f8c6b5"}, // scaled from 2 pods to 1
		{UID: "w2", Owner: "ReplicaSet/web-7d9f8c6b5", Revision: "7d9f8c6b5"},
		{UID: "s1", Owner: "StatefulSet/db", Revision: "db-5f6c"},
	}
	after := []kube.Pod{
		{UID: "w1", Owner: "ReplicaSet/web-7d9f8c6b5", Revision: "7d9f8c6b5"},
		{UID: "s2", Owner: "StatefulSet/db", Revision: "db-5f6c"}, // deleted and recreated, same template
		{UID: "h2", Owner: "Job/hook-migrate", Phase: "Running"},  // the hook's new attempt
	}
	if got := replacedPods(before, after); len(got) != 0 {
		t.Errorf("replacedPods = %v, want none", got)
	}
}

func TestReplacedPodsStatefulSetRollout(t *testing.T) {
	before := []kube.Pod{{UID: "s1", Owner: "StatefulSet/db", Revision: "db-5f6c"}}
	after := []kube.Pod{{UID: "s2", Owner: "StatefulSet/db", Revision: "db-77aa"}}
	if got := replacedPods(before, after); !reflect.DeepEqual(got, []string{"StatefulSet/db"}) {
		t.Errorf("replacedPods = %v, want [StatefulSet/db]", got)
	}
}

func TestServing(t *testing.T) {
	web := kube.Workload{Kind: "Deployment", Name: "web", PodLabels: map[string]string{"app": "web", "tier": "front"}}
	db := kube.Workload{Kind: "StatefulSet", Name: "db", PodLabels: map[string]string{"app": "db"}}
	services := []kube.Service{
		{Name: "web", Selector: map[string]string{"app": "web"}},
		{Name: "db", Selector: map[string]string{"app": "db"}},
	}
	got := serving([]kube.Workload{web, db}, services, "web:80")
	if len(got) != 1 || got[0].Name != "web" {
		t.Errorf("serving(web:80) = %v, want only web", got)
	}
	if got := serving([]kube.Workload{web, db}, services, "missing:80"); len(got) != 2 {
		t.Errorf("unknown Service should fall back to every workload, got %v", got)
	}
}

func TestOutageText(t *testing.T) {
	if got := outageText(0); got != "under a second" {
		t.Errorf("outageText(0) = %q", got)
	}
}

// A workload deleted and created again from the same template keeps its name and revision; its
// owner's UID changes. Scaling within one owner is still no restart.
func TestReplacedPodsSeesARecreatedWorkload(t *testing.T) {
	before := []kube.Pod{
		{UID: "a", Owner: "StatefulSet/db", OwnerUID: "sts-1", Revision: "db-5f6c"},
		{UID: "b", Owner: "ReplicaSet/web-7d9f8c6b5", OwnerUID: "rs-1", Revision: "7d9f8c6b5"},
		{UID: "c", Owner: "ReplicaSet/web-7d9f8c6b5", OwnerUID: "rs-1", Revision: "7d9f8c6b5"},
	}
	after := []kube.Pod{
		{UID: "a2", Owner: "StatefulSet/db", OwnerUID: "sts-2", Revision: "db-5f6c"},
		{UID: "b", Owner: "ReplicaSet/web-7d9f8c6b5", OwnerUID: "rs-1", Revision: "7d9f8c6b5"},
	}
	want := []string{"StatefulSet/db"}
	if got := replacedPods(before, after); !reflect.DeepEqual(got, want) {
		t.Errorf("replacedPods = %v, want %v", got, want)
	}
}
