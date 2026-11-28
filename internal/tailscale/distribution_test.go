package tailscale

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emaori/ts-funnel-service/internal/updater"
	"tailscale.com/clientupdate/distsign"
)

func elfFixture(machine uint16) []byte {
	b := make([]byte, 64)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(b[16:], 2)
	binary.LittleEndian.PutUint16(b[18:], machine)
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint16(b[52:], 64)
	return b
}
func archiveFixture(t *testing.T, kind string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"tailscale", "tailscaled"} {
		body := elfFixture(62)
		if kind == "architecture" {
			body = elfFixture(183)
		}
		header := &tar.Header{Name: "tailscale_1.104.1_amd64/" + name, Size: int64(len(body)), Mode: 0755, Typeflag: tar.TypeReg}
		if kind == "traversal" {
			header.Name = "../../escape"
		}
		if kind == "link" {
			header.Typeflag = tar.TypeSymlink
			header.Linkname = "/tmp/escape"
			header.Size = 0
			body = nil
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func signedFixture(t *testing.T, archive []byte) (map[string][]byte, []ed25519.PublicKey) {
	t.Helper()
	rootPriv, rootPub, err := distsign.GenerateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	root, err := distsign.ParseRootKey(rootPriv)
	if err != nil {
		t.Fatal(err)
	}
	signPriv, signPub, err := distsign.GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := distsign.ParseSigningKey(signPriv)
	if err != nil {
		t.Fatal(err)
	}
	bundleSig, err := root.SignSigningKeys(signPub)
	if err != nil {
		t.Fatal(err)
	}
	h := distsign.NewPackageHash()
	_, _ = h.Write(archive)
	sig, err := signer.SignPackageHash(h.Sum(nil), h.Len())
	if err != nil {
		t.Fatal(err)
	}
	roots, err := distsign.ParseRootKeyBundle(rootPub)
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{
		"/distsign.pub": signPub, "/distsign.pub.sig": bundleSig,
		"/stable/tailscale_1.104.1_amd64.tgz": archive, "/stable/tailscale_1.104.1_amd64.tgz.sig": sig,
	}, roots
}
func TestSignedArtifactsAndExtractionBoundary(t *testing.T) {
	for _, kind := range []string{"valid", "tampered", "untrusted-root", "traversal", "link", "architecture"} {
		t.Run(kind, func(t *testing.T) {
			archive := archiveFixture(t, kind)
			responses, roots := signedFixture(t, archive)
			if kind == "tampered" {
				responses["/stable/tailscale_1.104.1_amd64.tgz"] = append([]byte("tamper"), archive...)
			}
			if kind == "untrusted-root" {
				roots = []ed25519.PublicKey{make([]byte, ed25519.PublicKeySize)}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, ok := responses[r.URL.Path]
				if !ok {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write(b)
			}))
			defer server.Close()
			d := &Distribution{base: server.URL, arch: "amd64", client: server.Client(), roots: roots}
			dir := t.TempDir()
			err := d.Prepare(context.Background(), updater.Release{Version: "v1.104.1", Artifact: "stable/tailscale_1.104.1_amd64.tgz"}, dir)
			if kind == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(dir, "tailscaled")); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unsafe candidate accepted")
			} else if kind == "tampered" || kind == "untrusted-root" {
				if updater.ErrorCode(err, "") != "signature" {
					t.Fatalf("wrong failure category: %v", err)
				}
				if _, err := os.Stat(filepath.Join(dir, "tailscale")); !os.IsNotExist(err) {
					t.Fatal("unverified artifact extracted")
				}
			}
		})
	}
}
func TestMetadataConditionalRequestAndArchitecture(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("If-None-Match") == `"stable-1"` {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", `"stable-1"`)
		_ = json.NewEncoder(w).Encode(map[string]any{"TarballsVersion": "1.104.1", "Tarballs": map[string]string{"amd64": "tailscale_1.104.1_amd64.tgz"}})
	}))
	defer server.Close()
	d := &Distribution{base: server.URL, arch: "amd64", client: server.Client()}
	for i := 0; i < 2; i++ {
		r, err := d.Discover(context.Background())
		if err != nil || r.Version != "v1.104.1" {
			t.Fatal(r, err)
		}
	}
	if requests.Load() != 2 {
		t.Fatal("conditional request was not sent")
	}
	d.arch = "arm64"
	d.etag = ""
	d.cached = nil
	if _, err := d.Discover(context.Background()); err == nil {
		t.Fatal("missing architecture accepted")
	}
}
func TestMetadataHonorsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	d := &Distribution{base: server.URL, arch: "amd64", client: server.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := d.Discover(ctx); err == nil {
		t.Fatal("request ignored timeout")
	}
}
func TestOfficialDistribution(t *testing.T) {
	if os.Getenv("TS_FUNNEL_ONLINE_TEST") != "1" {
		t.Skip("explicit opt-in for official signed artifact download")
	}
	d, err := NewDistribution()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	release, err := d.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := d.Prepare(ctx, release, dir); err != nil {
		t.Fatal(err)
	}
	actual, err := BinaryVersion(ctx, dir)
	if err != nil || actual != release.Version {
		t.Fatal(actual, release.Version, err)
	}
	t.Logf("Verified official signed binary pair %s", actual)
}
func TestPublicAddressFilter(t *testing.T) {
	for _, s := range []string{"100.64.0.1", "127.0.0.1", "10.1.2.3", "192.168.0.1", "169.254.1.1", "::1", "fd7a:115c:a1e0::1", "::ffff:100.64.0.1", "192.0.2.1"} {
		if publicAddress(parseAddress(t, s)) {
			t.Fatal("accepted private/test destination", s)
		}
	}
	if !publicAddress(parseAddress(t, "1.1.1.1")) {
		t.Fatal("public address rejected")
	}
	if _, err := NormalizeVersion("bad"); err == nil {
		t.Fatal("bad version accepted")
	}
	if v, err := NormalizeVersion("1.104.1-t9a522a978\n"); err != nil || !strings.HasPrefix(v, "v1.104.1") {
		t.Fatal(v, err)
	}
}
