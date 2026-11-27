package tailscale

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"debug/elf"
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/emaori/ts-funnel-service/internal/updater"
	"tailscale.com/clientupdate/distsign"
)

// These public trust roots are copied verbatim from the pinned upstream module.
//
//go:embed roots/*.pem
var embeddedRoots embed.FS

type Distribution struct {
	base, arch string
	client     *http.Client
	roots      []ed25519.PublicKey
	mu         sync.Mutex
	etag       string
	cached     *updater.Release
}

func NewDistribution() (*Distribution, error) {
	d := &Distribution{base: "https://pkgs.tailscale.com", arch: runtime.GOARCH, client: &http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" || req.URL.User != nil {
				return errors.New("unsafe distribution redirect")
			}
			return nil
		},
	}}
	entries, err := embeddedRoots.ReadDir("roots")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		b, err := embeddedRoots.ReadFile("roots/" + entry.Name())
		if err != nil {
			return nil, err
		}
		keys, err := distsign.ParseRootKeyBundle(b)
		if err != nil {
			return nil, err
		}
		d.roots = append(d.roots, keys...)
	}
	if len(d.roots) == 0 {
		return nil, errors.New("no distribution trust roots")
	}
	return d, nil
}
func (d *Distribution) fetch(ctx context.Context, p string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+"/"+p, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("package server returned HTTP %d for %s", resp.StatusCode, p)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("distribution response exceeds size limit")
	}
	return b, nil
}
func (d *Distribution) Discover(ctx context.Context) (updater.Release, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+"/stable/?mode=json&os=linux", nil)
	if err != nil {
		return updater.Release{}, err
	}
	if d.etag != "" {
		req.Header.Set("If-None-Match", d.etag)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return updater.Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && d.cached != nil {
		return *d.cached, nil
	}
	if resp.StatusCode != http.StatusOK {
		return updater.Release{}, fmt.Errorf("release metadata HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return updater.Release{}, err
	}
	if len(b) > 1<<20 {
		return updater.Release{}, errors.New("release metadata too large")
	}
	var manifest struct {
		TarballsVersion string
		Tarballs        map[string]string
	}
	if err := json.Unmarshal(b, &manifest); err != nil {
		return updater.Release{}, err
	}
	v, err := updater.ParseVersion(manifest.TarballsVersion)
	if err != nil {
		return updater.Release{}, err
	}
	if v.Minor%2 != 0 {
		return updater.Release{}, errors.New("stable metadata advertised an unstable release")
	}
	file := fmt.Sprintf("tailscale_%s_%s.tgz", strings.TrimPrefix(manifest.TarballsVersion, "v"), d.arch)
	if manifest.Tarballs[d.arch] != file {
		return updater.Release{}, fmt.Errorf("no matching official stable artifact for %s", d.arch)
	}
	r := updater.Release{Version: "v" + strings.TrimPrefix(manifest.TarballsVersion, "v"), Artifact: "stable/" + file}
	d.etag = resp.Header.Get("ETag")
	d.cached = &r
	return r, nil
}
func (d *Distribution) Prepare(ctx context.Context, r updater.Release, stage string) error {
	signing, err := d.fetch(ctx, "distsign.pub", 1<<20)
	if err != nil {
		return &updater.CodedError{Code: "signature", Err: err}
	}
	rootSig, err := d.fetch(ctx, "distsign.pub.sig", 1<<16)
	if err != nil {
		return &updater.CodedError{Code: "signature", Err: err}
	}
	if !distsign.VerifyAny(d.roots, signing, rootSig) {
		return &updater.CodedError{Code: "signature", Err: errors.New("release signing keys do not validate against trusted roots")}
	}
	keys, err := distsign.ParseSigningKeyBundle(signing)
	if err != nil {
		return &updater.CodedError{Code: "signature", Err: err}
	}
	expected := fmt.Sprintf("stable/tailscale_%s_%s.tgz", strings.TrimPrefix(r.Version, "v"), d.arch)
	if r.Artifact != expected {
		return &updater.CodedError{Code: "artifact_unavailable", Err: errors.New("unexpected artifact path")}
	}
	sig, err := d.fetch(ctx, r.Artifact+".sig", 1<<16)
	if err != nil {
		return &updater.CodedError{Code: "signature", Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+"/"+r.Artifact, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	const maxArtifact = 256 << 20
	if resp.StatusCode != 200 || resp.ContentLength > maxArtifact {
		return errors.New("invalid or oversized artifact response")
	}
	archive := filepath.Join(stage, "package.unverified")
	f, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(archive)
	h := distsign.NewPackageHash()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxArtifact+1))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if n > maxArtifact {
		return errors.New("artifact exceeds download limit")
	}
	message := binary.LittleEndian.AppendUint64(h.Sum(nil), uint64(n))
	if !distsign.VerifyAny(keys, message, sig) {
		return &updater.CodedError{Code: "signature", Err: errors.New("package signature verification failed")}
	}
	if err := Extract(archive, stage, strings.TrimSuffix(path.Base(r.Artifact), ".tgz"), d.arch); err != nil {
		return &updater.CodedError{Code: "extraction", Err: err}
	}
	return updater.RecordArtifact(stage, r.Version, d.arch, "official-signed", r.Artifact)
}
func Extract(archive, dst, top, arch string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: gz, N: (512 << 20) + 1}
	tr := tar.NewReader(limited)
	found := map[string]bool{}
	for entries := 0; ; entries++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if entries > 128 {
			return errors.New("too many archive entries")
		}
		if path.IsAbs(h.Name) || path.Clean(h.Name) != strings.TrimSuffix(h.Name, "/") || strings.Contains(h.Name, "\\") {
			return errors.New("unsafe archive path")
		}
		if h.Name != top+"/" && !strings.HasPrefix(h.Name, top+"/") {
			return errors.New("archive entry outside expected release directory")
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return errors.New("archive contains a link or special file")
		}
		name := path.Base(h.Name)
		if h.Name != top+"/"+name || (name != "tailscale" && name != "tailscaled") {
			continue
		}
		if found[name] || h.Size <= 0 || h.Size > 128<<20 {
			return errors.New("invalid binary archive entry")
		}
		p := filepath.Join(dst, name)
		out, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		if err == nil {
			err = out.Sync()
		}
		closeErr := out.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		bin, err := elf.Open(p)
		if err != nil {
			return err
		}
		machine := elf.EM_X86_64
		if arch == "arm64" {
			machine = elf.EM_AARCH64
		} else if arch != "amd64" {
			bin.Close()
			return errors.New("unsupported runtime architecture")
		}
		valid := bin.Machine == machine && (bin.Type == elf.ET_EXEC || bin.Type == elf.ET_DYN)
		bin.Close()
		if !valid {
			return errors.New("candidate binary architecture mismatch")
		}
		found[name] = true
	}
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return err
	}
	if limited.N <= 0 {
		return errors.New("archive exceeds extraction limit")
	}
	if !found["tailscale"] || !found["tailscaled"] {
		return errors.New("archive lacks the matching binary pair")
	}
	return nil
}
