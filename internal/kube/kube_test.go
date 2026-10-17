package kube

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

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
