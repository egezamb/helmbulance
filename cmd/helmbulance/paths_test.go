package main

import (
	"maps"
	"testing"
)

func TestLocalPaths(t *testing.T) {
	t.Setenv("HOME", "/home/ege")
	t.Setenv("HELMBULANCE_STATE_DIR", "/srv/hb-state")
	want := map[string]string{"/home/ege": "~", "/srv/hb-state": "$HELMBULANCE_STATE_DIR"}
	if got := localPaths(); !maps.Equal(got, want) {
		t.Errorf("localPaths() = %v, want %v", got, want)
	}

	// The default state dir is under $HOME and shows as ~/.helmbulance.
	t.Setenv("HELMBULANCE_STATE_DIR", "")
	want = map[string]string{"/home/ege": "~"}
	if got := localPaths(); !maps.Equal(got, want) {
		t.Errorf("localPaths() = %v, want %v", got, want)
	}

	// A trailing slash on either is dropped, or nothing the tools print would match.
	t.Setenv("HOME", "/home/ege/")
	t.Setenv("HELMBULANCE_STATE_DIR", "/srv/hb-state/")
	want = map[string]string{"/home/ege": "~", "/srv/hb-state": "$HELMBULANCE_STATE_DIR"}
	if got := localPaths(); !maps.Equal(got, want) {
		t.Errorf("localPaths() with trailing slashes = %v, want %v", got, want)
	}
}
