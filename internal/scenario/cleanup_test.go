package scenario

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egezamb/helmbulance/internal/execx"
	"github.com/egezamb/helmbulance/internal/helm"
	"github.com/egezamb/helmbulance/internal/kube"
)

// After a deadline or Ctrl-C, each scenario must still uninstall its release and delete its
// namespace, or a reused cluster (--cluster) fails the next run on the fixed names.
func TestCleanupRunsAfterCancel(t *testing.T) {
	for _, s := range []Scenario{UpgradeValues{}, ChartUpgrade{}, ZeroDowntime{}} {
		t.Run(s.Name(), func(t *testing.T) {
			bin, calls := t.TempDir(), filepath.Join(t.TempDir(), "calls")
			script := "#!/bin/sh\necho \"$(basename \"$0\") $*\" >> \"$FAKE_CALLS\"\n"
			for _, name := range []string{"helm", "kubectl"} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("FAKE_CALLS", calls)

			runner := &execx.Runner{}
			env := &Env{
				Runner:      runner,
				Helm:        &helm.Client{Runner: runner, Timeout: time.Minute},
				Kube:        &kube.Client{Runner: runner},
				Chart:       "./chart",
				FromVersion: "1.0.0",
				Log:         io.Discard,
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			s.Run(ctx, env)

			b, _ := os.ReadFile(calls)
			for _, want := range []string{"helm uninstall ", "kubectl --kubeconfig  delete namespace "} {
				if !strings.Contains(string(b), want) {
					t.Errorf("no %q after cancel; calls:\n%s", want, b)
				}
			}
		})
	}
}
