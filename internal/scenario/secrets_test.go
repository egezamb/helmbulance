package scenario

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/egezamb/helmbulance/internal/kube"
	"github.com/egezamb/helmbulance/internal/oracle"
)

func TestOwnedOnly(t *testing.T) {
	secrets := []kube.Secret{{Name: "app-auth"}, {Name: "written-by-job"}, {Name: "operator-cert"}}
	got := ownedOnly(secrets, map[string]bool{"app-auth": true, "not-installed-yet": true})
	if len(got) != 1 || got[0].Name != "app-auth" {
		t.Errorf("ownedOnly = %v, want only app-auth", got)
	}
}

func TestSplitAdded(t *testing.T) {
	changes := []oracle.Change{
		{Ref: "app/jwt", Kind: oracle.Changed},
		{Ref: "app/new", Kind: oracle.Added},
		{Ref: "app/old", Kind: oracle.Removed},
	}
	lost, added := splitAdded(changes)
	if want := []oracle.Change{changes[0], changes[2]}; !reflect.DeepEqual(lost, want) {
		t.Errorf("lost = %v, want %v", lost, want)
	}
	if want := []oracle.Change{changes[1]}; !reflect.DeepEqual(added, want) {
		t.Errorf("added = %v, want %v", added, want)
	}
}

func TestNotes(t *testing.T) {
	if got := withNotes("ok", note("added", nil), ""); got != "ok" {
		t.Errorf("withNotes with nothing to say = %q, want the summary unchanged", got)
	}
	added := describe([]oracle.Change{{Ref: "a/x", Kind: oracle.Added}}, map[string][]string{"a/x": {"Deployment/web"}})
	got := withNotes("ok", note("keys added", added), note("not compared", []string{"`late`"}))
	if want := "ok; keys added: `a/x` added (read by Deployment/web); not compared: `late`"; got != want {
		t.Errorf("withNotes = %q, want %q", got, want)
	}
}

func TestSecretsInconclusive(t *testing.T) {
	notSettled := fmt.Errorf("%w after 10 reads", oracle.ErrNotSettled)
	if got := secretsInconclusive(Result{}, "after install", notSettled).Summary; !strings.HasPrefix(got, "the release's Secrets kept changing after install") {
		t.Errorf("summary for unsettled Secrets = %q", got)
	}
	if got := secretsInconclusive(Result{}, "after install", errors.New("boom")).Summary; !strings.HasPrefix(got, "could not read Secrets after install") {
		t.Errorf("summary for a read error = %q", got)
	}
}

// Chart C: v2 renames password to db-password and carries the value over, but its Deployment
// still reads password (optional: true), so the app starts without it. That is not a move.
func TestUnpairStillRead(t *testing.T) {
	moved := []oracle.Change{
		{Ref: "db/password -> db/db-password", Kind: oracle.MovedKind},
		{Ref: "old-name/key -> new-name/key", Kind: oracle.MovedKind},
		{Ref: "env/token -> env/api-token", Kind: oracle.MovedKind},
	}
	consumers := map[string][]string{
		"db/password":  {"Deployment/app"},
		"new-name/key": {"Deployment/app"},
		"env/*":        {"Deployment/worker"}, // envFrom: the env var name follows the key
	}
	kept, back := unpairStillRead(moved, consumers)
	if want := []oracle.Change{moved[1]}; !reflect.DeepEqual(kept, want) {
		t.Errorf("kept = %v, want %v", kept, want)
	}
	want := []oracle.Change{
		{Ref: "db/password", Kind: oracle.Removed}, {Ref: "db/db-password", Kind: oracle.Added},
		{Ref: "env/token", Kind: oracle.Removed}, {Ref: "env/api-token", Kind: oracle.Added},
	}
	if !reflect.DeepEqual(back, want) {
		t.Errorf("back = %v, want %v", back, want)
	}
}

func TestSplitExpected(t *testing.T) {
	changes := []oracle.Change{
		{Ref: "hb-upgrade-webhook-tls/tls.crt", Kind: oracle.Changed},
		{Ref: "hb-upgrade-webhook-tls/tls.key", Kind: oracle.Changed},
		{Ref: "hb-upgrade-db/password", Kind: oracle.Changed},
	}
	rest, expected := splitExpected(changes, []string{"*-webhook-tls/*"})
	if len(expected) != 2 || len(rest) != 1 || rest[0].Ref != "hb-upgrade-db/password" {
		t.Errorf("rest = %v, expected = %v", rest, expected)
	}
	if rest, expected := splitExpected(changes, nil); len(rest) != 3 || len(expected) != 0 {
		t.Errorf("no patterns: rest = %v, expected = %v", rest, expected)
	}
	// A glob does not cross the "/" between the Secret and the key.
	if _, expected := splitExpected(changes, []string{"*"}); len(expected) != 0 {
		t.Errorf(`"*" matched %v; it should match no "secret/key" reference`, expected)
	}
	// A removed key is not a rotation, so a matching pattern does not silence it (review item 5).
	removed := []oracle.Change{{Ref: "hb-upgrade-webhook-tls/tls.crt", Kind: oracle.Removed}}
	if rest, expected := splitExpected(removed, []string{"*-webhook-tls/*"}); len(rest) != 1 || len(expected) != 0 {
		t.Errorf("a removed key under a rotation pattern: rest = %v, expected = %v", rest, expected)
	}
}
