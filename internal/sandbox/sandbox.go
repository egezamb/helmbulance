// Package sandbox creates and removes the disposable kind clusters that scenarios run in.
//
// Helmbulance only ever talks to clusters it created. Each one gets its own kubeconfig under the
// state directory (never the user's ~/.kube/config), and the SHA-256 of the cluster's CA is
// recorded at creation. Reusing a cluster checks that fingerprint, so a kubeconfig pointing at
// some other cluster is refused.
package sandbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/egezamb/helmbulance/internal/execx"
)

// NamePrefix marks clusters created by Helmbulance.
const NamePrefix = "helmbulance-"

// Cluster is a sandbox cluster and the files that describe it.
type Cluster struct {
	Name       string
	Kubeconfig string
	dir        string
}

// StateDir is where cluster kubeconfigs and fingerprints are kept.
func StateDir() (string, error) {
	if d := os.Getenv("HELMBULANCE_STATE_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".helmbulance"), nil
}

// NewCluster reserves a cluster with a random name and its own state directory, before any kind
// command runs. The caller prints the name first, so a Ctrl-C during creation still leaves it on
// screen for `kind delete cluster --name`.
func NewCluster() (*Cluster, error) {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return nil, err
	}
	c, err := clusterFiles(NamePrefix + hex.EncodeToString(suffix))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return nil, err
	}
	return c, nil
}

// Create brings up the reserved cluster with kind and records its CA fingerprint. On failure it
// removes the cluster's state directory.
func (c *Cluster) Create(ctx context.Context, r *execx.Runner, nodeImage string) error {
	args := []string{"create", "cluster", "--name", c.Name, "--kubeconfig", c.Kubeconfig, "--wait", "180s"}
	if nodeImage != "" {
		args = append(args, "--image", nodeImage)
	}
	if _, err := r.Run(ctx, "kind", args...); err != nil {
		_ = os.RemoveAll(c.dir)
		return err
	}
	if err := os.Chmod(c.Kubeconfig, 0o600); err != nil {
		return err
	}
	fp, err := caFingerprint(c.Kubeconfig)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.dir, "ca.sha256"), []byte(fp), 0o600)
}

// Open returns an existing cluster that Helmbulance created, after checking its CA fingerprint.
func Open(name string) (*Cluster, error) {
	if !strings.HasPrefix(name, NamePrefix) {
		return nil, fmt.Errorf("cluster %q was not created by helmbulance (name must start with %q)", name, NamePrefix)
	}
	c, err := clusterFiles(name)
	if err != nil {
		return nil, err
	}
	want, err := os.ReadFile(filepath.Join(c.dir, "ca.sha256"))
	if err != nil {
		return nil, fmt.Errorf("no record of cluster %q in %s: %w", name, c.dir, err)
	}
	got, err := caFingerprint(c.Kubeconfig)
	if err != nil {
		return nil, err
	}
	if got != string(want) {
		return nil, errors.New("cluster CA does not match the one recorded at creation; refusing to use it")
	}
	return c, nil
}

// Delete removes the cluster and its files.
func (c *Cluster) Delete(ctx context.Context, r *execx.Runner) error {
	if _, err := r.Run(ctx, "kind", "delete", "cluster", "--name", c.Name, "--kubeconfig", c.Kubeconfig); err != nil {
		return err
	}
	return os.RemoveAll(c.dir)
}

func clusterFiles(name string) (*Cluster, error) {
	state, err := StateDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(state, "clusters", name)
	return &Cluster{Name: name, Kubeconfig: filepath.Join(dir, "kubeconfig"), dir: dir}, nil
}

// caFingerprint hashes the certificate-authority-data line of a kind kubeconfig. kind writes a
// single cluster entry, so the first match is the cluster's CA.
func caFingerprint(kubeconfig string) (string, error) {
	data, err := os.ReadFile(kubeconfig)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "certificate-authority-data:"); ok {
			sum := sha256.Sum256([]byte(strings.TrimSpace(v)))
			return hex.EncodeToString(sum[:]), nil
		}
	}
	return "", fmt.Errorf("%s has no certificate-authority-data", kubeconfig)
}
