package kube

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/egezamb/helmbulance/internal/execx"
)

// fakeKubectl puts a kubectl that prints out first on PATH.
func fakeKubectl(t *testing.T, out string) *Client {
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

func TestServicesReadsTypeAndAddresses(t *testing.T) {
	c := fakeKubectl(t, `{"items":[
		{"metadata":{"name":"web"},"spec":{"type":"ClusterIP","clusterIP":"10.96.0.10","selector":{"app":"web"},"ports":[{"name":"http","port":80}]}},
		{"metadata":{"name":"ext"},"spec":{"type":"ExternalName","externalIPs":["203.0.113.5"],"selector":{"app":"web"}}},
		{"metadata":{"name":"manual"},"spec":{"type":"ClusterIP","clusterIP":"10.96.0.11"}}
	]}`)
	got, err := c.Services(t.Context(), "ns")
	if err != nil {
		t.Fatal(err)
	}
	want := []Service{
		{Name: "web", Selector: map[string]string{"app": "web"}, Ports: []ServicePort{{Name: "http", Port: 80}},
			Type: "ClusterIP", ClusterIP: "10.96.0.10"},
		{Name: "ext", Selector: map[string]string{"app": "web"}, Type: "ExternalName", ExternalIPs: []string{"203.0.113.5"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Services =\n%+v\nwant\n%+v", got, want)
	}
}

func TestStatefulWorkloads(t *testing.T) {
	c := fakeKubectl(t, `{"items":[
		{"kind":"StatefulSet","metadata":{"name":"db"},"spec":{"volumeClaimTemplates":[{"metadata":{"name":"data"}}]}},
		{"kind":"StatefulSet","metadata":{"name":"cache"},"spec":{"template":{"spec":{"volumes":[{"name":"tmp","emptyDir":{}}]}}}},
		{"kind":"Deployment","metadata":{"name":"single-db"},"spec":{"template":{"spec":{"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"d"}}]}}}},
		{"kind":"Deployment","metadata":{"name":"web"},"spec":{"template":{"spec":{"volumes":[{"name":"cfg","secret":{"secretName":"s"}}]}}}}
	]}`)
	got, err := c.StatefulWorkloads(t.Context(), "ns")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"StatefulSet/db": true, "Deployment/single-db": true}; !reflect.DeepEqual(got, want) {
		t.Errorf("StatefulWorkloads = %v, want %v", got, want)
	}
}

func TestPVCs(t *testing.T) {
	c := fakeKubectl(t, `{"items":[
		{"metadata":{"name":"data-db-0","uid":"u1"},"status":{"phase":"Bound"}},
		{"metadata":{"name":"cache","uid":"u2"},"status":{"phase":"Pending"}}]}`)
	got, err := c.PVCs(t.Context(), "ns")
	if err != nil {
		t.Fatal(err)
	}
	if want := []PVC{{Name: "cache", UID: "u2", Phase: "Pending"}, {Name: "data-db-0", UID: "u1", Phase: "Bound"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("PVCs = %v, want %v", got, want)
	}
}

func TestRunWithPVC(t *testing.T) {
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	// Records every call; "logs" prints what the pod wrote.
	script := "#!/bin/sh\necho \"$*\" >> " + calls + "\ncase \"$*\" in *\" logs \"*) echo sentinel=abc ;; *jsonpath=*) printf Succeeded ;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	c := &Client{Runner: &execx.Runner{}}
	out, err := c.RunWithPVC(t.Context(), "ns", "hb-sentinel", "busybox", "data-db-0", "cat /pvc/x", time.Minute)
	if err != nil || out != "sentinel=abc\n" {
		t.Fatalf("RunWithPVC = %q, %v", out, err)
	}
	log, _ := os.ReadFile(calls)
	for _, want := range []string{`"claimName":"data-db-0"`, `"mountPath":"/pvc"`, `"automountServiceAccountToken":false`,
		`"drop":["ALL"]`, `"add":["CHOWN","DAC_OVERRIDE","FOWNER"]`, `"readOnlyRootFilesystem":true`,
		"get pod hb-sentinel -o jsonpath={.status.phase}", "delete pod hb-sentinel"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("kubectl calls do not contain %q:\n%s", want, log)
		}
	}
}

func TestClaimUsers(t *testing.T) {
	c := fakeKubectl(t, `{"items":[
		{"kind":"StatefulSet","metadata":{"name":"db"},"spec":{"volumeClaimTemplates":[{"metadata":{"name":"data"}}]}},
		{"kind":"Deployment","metadata":{"name":"app"},"spec":{"template":{"spec":{"volumes":[{"name":"d","persistentVolumeClaim":{"claimName":"app-data"}}]}}}},
		{"kind":"Deployment","metadata":{"name":"web"},"spec":{"template":{"spec":{"volumes":[{"name":"c","emptyDir":{}}]}}}}
	]}`)
	got, err := c.ClaimUsers(t.Context(), "ns", []string{"data-db-0", "app-data", "orphan"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"data-db-0": {"StatefulSet/db"}, "app-data": {"Deployment/app"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ClaimUsers = %v, want %v", got, want)
	}
}

func TestRunWithPVCStopsOnAFailedPod(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in *jsonpath=*) printf Failed ;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	c := &Client{Runner: &execx.Runner{}}
	start := time.Now()
	_, err := c.RunWithPVC(t.Context(), "ns", "hb-sentinel", "busybox", "data", "true", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "is Failed") || time.Since(start) > 10*time.Second {
		t.Errorf("RunWithPVC on a failed pod = %v after %s; want a quick error naming the phase", err, time.Since(start))
	}
}
