package scenario

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// busyboxImage runs Helmbulance's own helper pods (traffic, sentinels). Pinned by the digest of
// the multi-architecture index, so amd64 and arm64 hosts get the same image.
const busyboxImage = "busybox:1.36.1@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662"

// sentinelImage writes and reads the sentinel files.
const sentinelImage = busyboxImage

// sentinelFile is where the sentinel goes inside each volume. The name says who wrote it, in case
// someone finds it in their own data.
const sentinelFile = "/pvc/.helmbulance-sentinel"

// newSentinel returns a random value, so a volume that was emptied and refilled cannot match.
func newSentinel() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// sentinelTimeout bounds each sentinel pod; it only pulls a small image and mounts a volume.
const sentinelTimeout = 2 * time.Minute

// sentinelPod names a sentinel pod uniquely, so a pod left behind by an earlier run in a reused
// cluster cannot clash with it.
func sentinelPod(step string, i int) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("hb-sentinel-%s%d-%s", step, i, hex.EncodeToString(b))
}

// writeSentinelScript writes value into the volume, gives the file the volume's owner so an app
// that checks ownership is not upset by it, and prints "written", or "write-failed" when the
// volume is read-only or full.
func writeSentinelScript(value string) string {
	return "if printf %s " + value + " > " + sentinelFile + "; then chown $(stat -c %u:%g /pvc) " + sentinelFile +
		" 2>/dev/null; echo written; else echo write-failed; fi"
}

// readSentinelScript prints the sentinel, or "missing". The control read before the uninstall
// leaves the file; the last read removes it, so nothing of Helmbulance stays in the data.
func readSentinelScript(remove bool) string {
	s := "cat " + sentinelFile + " 2>/dev/null || echo missing"
	if remove {
		s += "; rm -f " + sentinelFile
	}
	return s
}

// sentinelState compares what the read pod printed with the value written.
func sentinelState(out, want string) string {
	switch got := strings.TrimSpace(out); got {
	case want:
		return "kept"
	case "missing", "":
		return "missing"
	default:
		return "replaced"
	}
}
