package scenario

import (
	"testing"

	"github.com/egezamb/helmbulance/internal/oracle"
)

func TestChangeSeverity(t *testing.T) {
	consumers := map[string][]string{
		"db/password":   {"StatefulSet/db"},
		"app/*":         {"Deployment/web"},
		"webhook-tls/*": {"Deployment/webhook"},
	}
	stateful := map[string]bool{"StatefulSet/db": true}
	for ref, want := range map[string]string{
		"db/password":         "high",   // a credential the database keeps
		"app/jwtSecret":       "medium", // a credential, but no persistent data reads it
		"db/username":         "medium", // not a credential
		"webhook-tls/tls.crt": "low",    // TLS material
		"orphan/password":     "medium", // read by nothing we know of
	} {
		if got := changeSeverity(oracle.Change{Ref: ref, Kind: oracle.Changed}, consumers, stateful, true); got != want {
			t.Errorf("changeSeverity(%s) = %s, want %s", ref, got, want)
		}
	}
}

// When the stateful workloads could not be listed, a credential is not lowered from high, and a
// note says so (review item 4). TLS and non-credentials are unaffected.
func TestChangeSeverityStatefulUnknown(t *testing.T) {
	consumers := map[string][]string{"app/*": {"Deployment/web"}}
	for ref, want := range map[string]string{
		"app/password":  "high",   // kept high: a persistent reader cannot be ruled out
		"app/jwtSecret": "high",   // same, even with no known stateful reader
		"app/username":  "medium", // not a credential
		"app/tls.crt":   "low",    // TLS is always low
	} {
		if got := changeSeverity(oracle.Change{Ref: ref, Kind: oracle.Changed}, consumers, nil, false); got != want {
			t.Errorf("changeSeverity(%s, unknown) = %s, want %s", ref, got, want)
		}
	}
	cred := []oracle.Change{{Ref: "app/password", Kind: oracle.Changed}}
	if got := unknownStatefulNote(false, cred); got == "" {
		t.Error("unknownStatefulNote(false, credential) = empty, want a warning")
	}
	if got := unknownStatefulNote(true, cred); got != "" {
		t.Errorf("unknownStatefulNote(true, ...) = %q, want empty when the listing succeeded", got)
	}
	tls := []oracle.Change{{Ref: "app/tls.crt", Kind: oracle.Changed}}
	if got := unknownStatefulNote(false, tls); got != "" {
		t.Errorf("unknownStatefulNote(false, no credential) = %q, want empty", got)
	}
}

func TestWorstSeverityAndOnlyTLS(t *testing.T) {
	consumers := map[string][]string{"db/*": {"StatefulSet/db"}}
	stateful := map[string]bool{"StatefulSet/db": true}
	tls := []oracle.Change{{Ref: "ca/tls.crt", Kind: oracle.Changed}, {Ref: "ca/tls.key", Kind: oracle.Changed}}
	if got := worstSeverity(tls, consumers, stateful, true); got != "low" || !onlyTLS(tls) {
		t.Errorf("TLS only: worst = %s, onlyTLS = %v; want low, true", got, onlyTLS(tls))
	}
	mixed := append(tls, oracle.Change{Ref: "db/password", Kind: oracle.Removed})
	if got := worstSeverity(mixed, consumers, stateful, true); got != "high" || onlyTLS(mixed) {
		t.Errorf("mixed: worst = %s, onlyTLS = %v; want high, false", got, onlyTLS(mixed))
	}
}

func TestDescribeNamesTheClass(t *testing.T) {
	got := describe([]oracle.Change{
		{Ref: "db/password", Kind: oracle.Changed},
		{Ref: "ca/tls.crt", Kind: oracle.Removed},
		{Ref: "db/username", Kind: oracle.Changed},
	}, map[string][]string{"db/*": {"StatefulSet/db"}})
	want := []string{
		"`db/password` changed, credential (read by StatefulSet/db)",
		"`ca/tls.crt` removed, tls",
		"`db/username` changed (read by StatefulSet/db)",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("describe[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
