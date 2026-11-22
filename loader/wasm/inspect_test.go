package wasm

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const inspectionManifest = `{"id":"inspect.safe","name":"Safe","version":"1","provides":{"functions":[{"name":"ping"}]},"tools":[{"name":"echo"}]}`

type inspectionZipEntry struct {
	name string
	data []byte
	mode fs.FileMode
}

func inspectionZip(t *testing.T, entries ...inspectionZipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range entries {
		fh := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.mode != 0 {
			fh.SetMode(entry.mode)
		}
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func inspectionFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestInspectPackageReturnsManifestAndAssetMetadata(t *testing.T) {
	module := inspectionFixture(t, "inspect.wasm")
	asset := []byte("export const answer = 42\n")
	data := inspectionZip(t,
		inspectionZipEntry{name: "manifest.json", data: []byte(inspectionManifest)},
		inspectionZipEntry{name: "plugin.wasm", data: module},
		inspectionZipEntry{name: "assets/panel.js", data: asset},
	)
	got, err := InspectPackage(context.Background(), data, "staged.egop.zip", Options{})
	if err != nil {
		t.Fatalf("InspectPackage: %v", err)
	}
	if got.Manifest.ID != "inspect.safe" || got.WASMSize != int64(len(module)) {
		t.Fatalf("inspection = %+v", got)
	}
	if got.EntryCount != 3 || got.TotalBytes != int64(len(inspectionManifest)+len(module)+len(asset)) {
		t.Fatalf("package bounds metadata = %+v", got)
	}
	if len(got.Assets) != 1 {
		t.Fatalf("assets = %+v", got.Assets)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256(asset))
	a := got.Assets[0]
	if a.Name != "panel.js" || a.Size != int64(len(asset)) || a.SHA256 != wantHash {
		t.Fatalf("asset = %+v", a)
	}
}

func TestInspectPackageRequiresManifestAndWasm(t *testing.T) {
	module := inspectionFixture(t, "inspect.wasm")
	cases := []struct {
		name    string
		entries []inspectionZipEntry
		want    string
	}{
		{name: "manifest", entries: []inspectionZipEntry{{name: "plugin.wasm", data: module}}, want: "manifest.json"},
		{name: "wasm", entries: []inspectionZipEntry{{name: "manifest.json", data: []byte(inspectionManifest)}}, want: "plugin.wasm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := InspectPackage(context.Background(), inspectionZip(t, tc.entries...), "staged.egop.zip", Options{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestInspectPackageRejectsMissingABI(t *testing.T) {
	data := inspectionZip(t,
		inspectionZipEntry{name: "manifest.json", data: []byte(inspectionManifest)},
		inspectionZipEntry{name: "plugin.wasm", data: inspectionFixture(t, "inspect_no_alloc.wasm")},
	)
	_, err := InspectPackage(context.Background(), data, "staged.egop.zip", Options{})
	if err == nil || !strings.Contains(err.Error(), ExportHostAlloc) {
		t.Fatalf("missing alloc error = %v", err)
	}
}

func TestInspectPackageDoesNotRunStart(t *testing.T) {
	data := inspectionZip(t,
		inspectionZipEntry{name: "manifest.json", data: []byte(`{"id":"inspect.trap","name":"Trap","version":"1"}`)},
		inspectionZipEntry{name: "plugin.wasm", data: inspectionFixture(t, "inspect_start_trap.wasm")},
	)
	if _, err := InspectPackage(context.Background(), data, "staged.egop.zip", Options{}); err != nil {
		t.Fatalf("inspection executed _start: %v", err)
	}
}

func TestInspectPackageNeverUsesDiskCompilationCache(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "wasm-cache")
	t.Setenv("EGOP_WASM_CACHE_DIR", cacheDir)
	data := inspectionZip(t,
		inspectionZipEntry{name: "manifest.json", data: []byte(`{"id":"inspect.cache","name":"Cache","version":"1"}`)},
		inspectionZipEntry{name: "plugin.wasm", data: inspectionFixture(t, "inspect_start_trap.wasm")},
	)
	if _, err := InspectPackage(context.Background(), data, "staged.egop.zip", Options{}); err != nil {
		t.Fatalf("InspectPackage: %v", err)
	}
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Fatalf("inspection touched disk compilation cache: %v", err)
	}
}

func TestInspectPackageEntryLimit(t *testing.T) {
	data := inspectionZip(t,
		inspectionZipEntry{name: "manifest.json", data: []byte(inspectionManifest)},
		inspectionZipEntry{name: "plugin.wasm", data: inspectionFixture(t, "inspect.wasm")},
		inspectionZipEntry{name: "assets/panel.js", data: []byte("x")},
	)
	_, err := InspectPackage(context.Background(), data, "staged.egop.zip", Options{MaxEntries: 2})
	if err == nil || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("entry limit error = %v", err)
	}
}

func TestLoadFSZipWithMaximumByteLimits(t *testing.T) {
	data := inspectionZip(t,
		inspectionZipEntry{name: "manifest.json", data: []byte(`{"id":"zip.limits","name":"Limits","version":"1"}`)},
		inspectionZipEntry{name: "assets/a.txt", data: []byte("a")},
	)
	p, err := LoadFS(context.Background(), data, "limits.egop.zip", Options{MaxEntryBytes: int64(1<<63 - 1), MaxTotalBytes: int64(1<<63 - 1)})
	if err != nil {
		t.Fatalf("maximum byte limits must not overflow: %v", err)
	}
	_ = p.Close(context.Background())
}

func TestLoadFSRejectsUnsafeZipEntries(t *testing.T) {
	manifest := []byte(`{"id":"zip.safe","name":"Safe","version":"1"}`)
	cases := []struct {
		name    string
		entries []inspectionZipEntry
	}{
		{name: "absolute", entries: []inspectionZipEntry{{name: "/tmp/evil", data: []byte("x")}}},
		{name: "windows absolute", entries: []inspectionZipEntry{{name: "C:/evil", data: []byte("x")}}},
		{name: "parent", entries: []inspectionZipEntry{{name: "assets/../../evil", data: []byte("x")}}},
		{name: "parent in ignored entry", entries: []inspectionZipEntry{{name: "other/../evil", data: []byte("x")}}},
		{name: "backslash", entries: []inspectionZipEntry{{name: `assets\evil`, data: []byte("x")}}},
		{name: "nul", entries: []inspectionZipEntry{{name: "assets/evil\x00.js", data: []byte("x")}}},
		{name: "duplicate", entries: []inspectionZipEntry{{name: "assets/panel.js", data: []byte("a")}, {name: "assets/panel.js", data: []byte("b")}}},
		{name: "case collision", entries: []inspectionZipEntry{{name: "assets/Panel.js", data: []byte("a")}, {name: "assets/panel.js", data: []byte("b")}}},
		{name: "symlink", entries: []inspectionZipEntry{{name: "assets/link", data: []byte("target"), mode: fs.ModeSymlink | 0o777}}},
		{name: "special file", entries: []inspectionZipEntry{{name: "assets/pipe", mode: fs.ModeNamedPipe | 0o600}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := []inspectionZipEntry{{name: "manifest.json", data: manifest}}
			entries = append(entries, tc.entries...)
			data := inspectionZip(t, entries...)
			if _, err := LoadFS(context.Background(), data, "unsafe.egop.zip", Options{}); err == nil {
				t.Fatal("unsafe ZIP entry was accepted")
			}
			entries = append(entries, inspectionZipEntry{name: "plugin.wasm", data: inspectionFixture(t, "inspect_start_trap.wasm")})
			if _, err := InspectPackage(context.Background(), inspectionZip(t, entries...), "unsafe.egop.zip", Options{}); err == nil {
				t.Fatal("inspection accepted unsafe ZIP entry")
			}
		})
	}
}

func TestLoadFSZipAllowsSafeDirectoriesAndIgnoredFiles(t *testing.T) {
	data := inspectionZip(t,
		inspectionZipEntry{name: "manifest.json", data: []byte(`{"id":"zip.dirs","name":"Dirs","version":"1"}`)},
		inspectionZipEntry{name: "assets/", mode: fs.ModeDir | 0o755},
		inspectionZipEntry{name: "assets/sub/", mode: fs.ModeDir | 0o755},
		inspectionZipEntry{name: "assets/sub/a.txt", data: []byte("a")},
		inspectionZipEntry{name: "metadata.txt", data: []byte("ignored safely")},
	)
	p, err := LoadFS(context.Background(), data, "dirs.egop.zip", Options{})
	if err != nil {
		t.Fatalf("valid ZIP with directory entries: %v", err)
	}
	defer p.Close(context.Background())
	if got := string(p.Assets()["sub/a.txt"]); got != "a" {
		t.Fatalf("asset = %q", got)
	}
}
