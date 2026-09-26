package dataplane

import (
	"context"
	"errors"
	"github.com/shengyu/edgelink/internal/model"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewVerifyMustCheckRuntimeAndVersion(t *testing.T) {
	h := NewHAProxy()
	h.SetFileReader(func(path string) ([]byte, error) {
		if filepath.Base(path) == VersionFileName {
			return []byte("1"), nil
		}
		if path == "/proc/net/tcp" {
			return []byte("sl local_address rem_address st\n0: 00000000:01BB 00000000:0000 0A\n"), nil
		}
		return nil, os.ErrNotExist
	})
	vr, err := h.Verify(context.Background(), VerifyRequest{Version: 2, State: model.DesiredState{SNIEntries: []model.SNIEntry{{Enabled: true, BindAddr: "0.0.0.0", BindPort: 443}}}})
	if err != nil {
		t.Fatal(err)
	}
	if vr.OK {
		t.Fatalf("accepted version %d when requested 2, without checking owning process; OwnedByUs=%v", vr.DataplaneVersion, vr.Listeners[0].OwnedByUs)
	}
}

func TestReviewPartialPublishMustRestoreConfig(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "current")
	candidate := filepath.Join(root, "v2")
	os.MkdirAll(current, 0750)
	os.MkdirAll(candidate, 0750)
	os.WriteFile(filepath.Join(current, "haproxy.cfg"), []byte("old-config"), 0640)
	os.WriteFile(filepath.Join(current, VersionFileName), []byte("1"), 0640)
	os.WriteFile(filepath.Join(candidate, "haproxy.cfg"), []byte("new-config"), 0640)
	os.WriteFile(filepath.Join(candidate, "meta.json"), []byte("{}"), 0640)
	h := NewHAProxy()
	h.CurrentDir = current
	h.SetRunner(func(context.Context, string, ...string) (string, string, error) { return "", "", nil })
	h.writeFile = func(path string, b []byte, m os.FileMode) error {
		if filepath.Base(path) == "meta.json" {
			return errors.New("injected disk failure")
		}
		return atomicWriteFile(path, b, m)
	}
	err := h.Apply(context.Background(), ApplyRequest{Version: 2, PreviousVersion: 1, ConfigDir: candidate, ConfigPath: filepath.Join(candidate, "haproxy.cfg")})
	if err == nil {
		t.Fatal("expected injected failure")
	}
	b, _ := os.ReadFile(filepath.Join(current, "haproxy.cfg"))
	v, _ := h.ActiveVersion(context.Background())
	if string(b) != "old-config" {
		t.Fatalf("failed apply left %q on disk but active marker still %d", b, v)
	}
}
