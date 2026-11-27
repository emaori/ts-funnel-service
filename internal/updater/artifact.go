package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

type ArtifactRecord struct {
	Version      string            `json:"version"`
	Architecture string            `json:"architecture"`
	Source       string            `json:"source"`
	Artifact     string            `json:"artifact"`
	SHA256       map[string]string `json:"sha256"`
}

// RecordArtifact runs only after distribution verification, or for the image's
// trusted seed. Hashes detect accidental changes to the persisted executable pair.
func RecordArtifact(dir, version, arch, source, artifact string) error {
	r := ArtifactRecord{Version: version, Architecture: arch, Source: source, Artifact: artifact, SHA256: map[string]string{}}
	for _, name := range []string{"tailscale", "tailscaled"} {
		hash, err := fileHash(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		r.SHA256[name] = hash
	}
	return AtomicJSON(filepath.Join(dir, "artifact-record"), r)
}
func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func verifyArtifact(dir, version string) error {
	var r ArtifactRecord
	if err := ReadJSON(filepath.Join(dir, "artifact-record"), &r); err != nil {
		return err
	}
	if r.Version != version || r.Architecture != runtime.GOARCH || (r.Source != "image" && r.Source != "official-signed") {
		return fmt.Errorf("invalid artifact provenance for %s", version)
	}
	for _, name := range []string{"tailscale", "tailscaled"} {
		hash, err := fileHash(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if hash != r.SHA256[name] {
			return fmt.Errorf("persisted executable changed: %s", name)
		}
	}
	return nil
}
