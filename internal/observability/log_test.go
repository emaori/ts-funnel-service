package observability

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogMirrorsRotatesAndRedacts(t *testing.T) {
	var out bytes.Buffer
	dir := t.TempDir()
	l := NewLogger(dir, 1024, 3, &out, "tskey-auth-sensitive")
	for i := 0; i < 40; i++ {
		l.Printf("phase=validating token=tskey-auth-sensitive %s", strings.Repeat("x", 200))
	}
	if !strings.Contains(out.String(), "[REDACTED]") || strings.Contains(out.String(), "tskey-auth-sensitive") {
		t.Fatal("stdout exposed secret")
	}
	files, e := os.ReadDir(dir)
	if e != nil {
		t.Fatal(e)
	}
	if len(files) != 3 {
		t.Fatal("retention limit not applied")
	}
	for _, f := range files {
		b, e := os.ReadFile(filepath.Join(dir, f.Name()))
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(string(b), "tskey-auth-sensitive") || len(b) > 1024 {
			t.Fatal("bad file retention/redaction")
		}
	}
}
func TestFileFailureKeepsStdout(t *testing.T) {
	var out bytes.Buffer
	dir := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(dir, []byte("x"), 0600)
	l := NewLogger(dir, 1024, 2, &out)
	l.Printf("result=rolled_back")
	if !strings.Contains(out.String(), "result=rolled_back") || !strings.Contains(out.String(), "file log unavailable") || l.Check() == nil {
		t.Fatal("log sink failure hidden")
	}
}
func TestOversizedLineIsBounded(t *testing.T) {
	var out bytes.Buffer
	dir := t.TempDir()
	l := NewLogger(dir, 1024, 2, &out)
	l.Printf("%s", strings.Repeat("x", 10000))
	b, e := os.ReadFile(filepath.Join(dir, "updates.log"))
	if e != nil {
		t.Fatal(e)
	}
	if len(b) > 1024 {
		t.Fatal("oversized line bypassed file cap")
	}
}
