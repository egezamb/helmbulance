package scenario

import (
	"context"
	"errors"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/egezamb/helmbulance/internal/kube"
	"github.com/egezamb/helmbulance/internal/oracle"
)

// settleInterval and settleReads bound how long a Secret snapshot waits for the namespace to stop
// changing: up to about half a minute after helm returns. Variables, so tests can shorten them.
var (
	settleInterval = 3 * time.Second
	settleReads    = 10
)

// secretReader snapshots the Secrets that the release itself renders, once two reads in a row
// agree. Secrets written by a Job, an operator or the app are left out, since they change for
// reasons the upgrade did not cause; their names are kept so the report can say what was not
// compared.
//
// owned grows with every snapshot and never shrinks: a "generate once" Secret is rendered by the
// install only (later renders find it with lookup), and helm.sh/resource-policy: keep leaves it in
// place, so it stays the release's Secret. One the release stops rendering without keep is
// deleted by helm, and the diff reports it as removed.
type secretReader struct {
	ctx         context.Context
	env         *Env
	release, ns string
	owned       map[string]bool
	notCompared map[string]bool
}

func newSecretReader(ctx context.Context, env *Env, release, ns string) *secretReader {
	return &secretReader{ctx: ctx, env: env, release: release, ns: ns, owned: map[string]bool{}, notCompared: map[string]bool{}}
}

func (r *secretReader) snapshot() (oracle.SecretSnapshot, error) {
	// What the release renders does not change while the namespace settles, so it is read once.
	rendered, err := r.env.Helm.OwnedSecrets(r.ctx, r.release, r.ns)
	if err != nil {
		return nil, err
	}
	maps.Copy(r.owned, rendered)
	owned := r.owned
	return oracle.Settle(r.ctx, func() (oracle.SecretSnapshot, error) {
		secrets, err := r.env.Kube.Secrets(r.ctx, r.ns)
		if err != nil {
			return nil, err
		}
		for _, s := range secrets {
			if !owned[s.Name] {
				r.notCompared[s.Name] = true
			}
		}
		return r.env.Hasher.Snapshot(ownedOnly(secrets, owned)), nil
	}, settleInterval, settleReads)
}

// notComparedNote names the Secrets that were in the namespace but never rendered by the release.
func (r *secretReader) notComparedNote() string {
	var names []string
	for _, n := range slices.Sorted(maps.Keys(r.notCompared)) {
		if !r.owned[n] {
			names = append(names, "`"+n+"`")
		}
	}
	return note("not compared, since the release does not render them", names)
}

// secretsInconclusive says whether reading the Secrets failed or they never stopped changing.
func secretsInconclusive(res Result, after string, err error) Result {
	if errors.Is(err, oracle.ErrNotSettled) {
		return inconclusive(res, "the release's Secrets kept changing "+after, err)
	}
	return inconclusive(res, "could not read Secrets "+after, err)
}

func ownedOnly(secrets []kube.Secret, owned map[string]bool) []kube.Secret {
	var out []kube.Secret
	for _, s := range secrets {
		if owned[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// splitAdded separates keys that appeared from keys that changed or went away. A new key breaks
// nothing that already uses the Secret, so it is not a finding.
func splitAdded(changes []oracle.Change) (lost, added []oracle.Change) {
	for _, c := range changes {
		if c.Kind == oracle.Added {
			added = append(added, c)
		} else {
			lost = append(lost, c)
		}
	}
	return lost, added
}

// note renders one item of a summary's notes, or nothing when there are no items.
func note(label string, items []string) string {
	if len(items) == 0 {
		return ""
	}
	return label + ": " + strings.Join(items, ", ")
}

// withNotes appends the non-empty notes to a summary. Notes report what is not a finding.
func withNotes(summary string, notes ...string) string {
	for _, n := range notes {
		if n != "" {
			summary += "; " + n
		}
	}
	return summary
}

// unpairStillRead undoes a move whose old key a workload still reads after the upgrade, through
// secretKeyRef or envFrom: the value moved, but that reader lost it. Those come back as a removed
// and an added key.
func unpairStillRead(moved []oracle.Change, consumers map[string][]string) (kept, back []oracle.Change) {
	for _, m := range moved {
		from, to, _ := strings.Cut(m.Ref, " -> ")
		secret, _, _ := strings.Cut(from, "/")
		if len(consumers[from]) > 0 || len(consumers[secret+"/*"]) > 0 {
			back = append(back, oracle.Change{Ref: from, Kind: oracle.Removed}, oracle.Change{Ref: to, Kind: oracle.Added})
			continue
		}
		kept = append(kept, m)
	}
	return kept, back
}

// noSecretsTested is said instead of "Secrets stayed the same" when no key was compared.
const noSecretsTested = "the release renders no Secrets; Secret drift was not tested"

// noKeys reports whether no snapshot holds a key, so no Secret was compared at all.
func noKeys(snapshots ...oracle.SecretSnapshot) bool {
	for _, s := range snapshots {
		if len(s) > 0 {
			return false
		}
	}
	return true
}

// splitExpected separates the changes that match an --expected-rotation pattern ("secret/key",
// with path.Match globs such as "*-webhook-tls/*"), so rotations the chart makes on purpose are
// named in the summary instead of becoming findings. A rotation is a changed value, so only a
// changed key is ever expected: a removed key the new render drops is still reported, even under a
// matching pattern (review item 5).
func splitExpected(changes []oracle.Change, patterns []string) (rest, expected []oracle.Change) {
	for _, c := range changes {
		if c.Kind == oracle.Changed && matchesAny(c.Ref, patterns) {
			expected = append(expected, c)
		} else {
			rest = append(rest, c)
		}
	}
	return rest, expected
}

func matchesAny(ref string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, ref); ok {
			return true
		}
	}
	return false
}
