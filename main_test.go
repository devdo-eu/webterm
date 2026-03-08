package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- shellInit ---

func TestShellInitPowerShell(t *testing.T) {
	for _, shell := range []string{"powershell.exe", "pwsh.exe", "C:\\Program Files\\PowerShell\\7\\pwsh.exe"} {
		got := shellInit(shell)
		if got == "" {
			t.Errorf("shellInit(%q) returned empty, want OSC 7 init", shell)
		}
		if !strings.Contains(got, "]7;") {
			t.Errorf("shellInit(%q) missing OSC 7 sequence", shell)
		}
	}
}

func TestShellInitCmd(t *testing.T) {
	got := shellInit("cmd.exe")
	if got == "" {
		t.Fatal("shellInit(cmd.exe) returned empty")
	}
	if !strings.Contains(got, "]7;") {
		t.Error("shellInit(cmd.exe) missing OSC 7 sequence")
	}
}

func TestShellInitUnknown(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "unknown"} {
		if got := shellInit(shell); got != "" {
			t.Errorf("shellInit(%q) = %q, want empty", shell, got)
		}
	}
}

// --- loadConfig ---

func TestLoadConfigDefaults(t *testing.T) {
	cfg := loadConfig()
	if cfg.Port != "1122" {
		t.Errorf("default port = %q, want 1122", cfg.Port)
	}
	if cfg.Shell != "powershell.exe" {
		t.Errorf("default shell = %q, want powershell.exe", cfg.Shell)
	}
}

// --- handleFiles ---

func TestHandleFilesListDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("hello"), 0644)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("world"), 0644)
	os.Mkdir(filepath.Join(dir, "subdir"), 0755)

	req := httptest.NewRequest("GET", "/api/files?path="+url.QueryEscape(dir), nil)
	w := httptest.NewRecorder()
	handleFiles(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp filesResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}

	if len(resp.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(resp.Entries))
	}

	// Directories first
	if !resp.Entries[0].IsDir || resp.Entries[0].Name != "subdir" {
		t.Errorf("first entry = %+v, want dir 'subdir'", resp.Entries[0])
	}

	// Then files sorted alphabetically
	if resp.Entries[1].Name != "a.txt" {
		t.Errorf("second entry name = %q, want a.txt", resp.Entries[1].Name)
	}
	if resp.Entries[2].Name != "b.txt" {
		t.Errorf("third entry name = %q, want b.txt", resp.Entries[2].Name)
	}
}

func TestHandleFilesInvalidDir(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/files?path="+url.QueryEscape("Z:\\nonexistent\\path"), nil)
	w := httptest.NewRecorder()
	handleFiles(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleFilesEmptyDir(t *testing.T) {
	dir := t.TempDir()

	req := httptest.NewRequest("GET", "/api/files?path="+url.QueryEscape(dir), nil)
	w := httptest.NewRecorder()
	handleFiles(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp filesResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Entries) != 0 {
		t.Errorf("entries = %d, want 0", len(resp.Entries))
	}
}

// --- handleFile (GET) ---

func TestHandleFileRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("file content"), 0644)

	req := httptest.NewRequest("GET", "/api/file?path="+url.QueryEscape(path), nil)
	w := httptest.NewRecorder()
	handleFile(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["content"] != "file content" {
		t.Errorf("content = %q, want 'file content'", resp["content"])
	}
	if resp["path"] != path {
		t.Errorf("path = %q, want %q", resp["path"], path)
	}
}

func TestHandleFileReadMissingPath(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/file", nil)
	w := httptest.NewRecorder()
	handleFile(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleFileReadNotFound(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/file?path="+url.QueryEscape("Z:\\no\\such\\file.txt"), nil)
	w := httptest.NewRecorder()
	handleFile(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleFileReadDirectory(t *testing.T) {
	dir := t.TempDir()

	req := httptest.NewRequest("GET", "/api/file?path="+url.QueryEscape(dir), nil)
	w := httptest.NewRecorder()
	handleFile(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleFileReadTooLarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.bin")
	os.WriteFile(path, make([]byte, 6<<20), 0644) // 6 MB > 5 MB limit

	req := httptest.NewRequest("GET", "/api/file?path="+url.QueryEscape(path), nil)
	w := httptest.NewRecorder()
	handleFile(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// --- handleFile (PUT) ---

func TestHandleFileWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("old"), 0644)

	body := strings.NewReader(`{"content":"new content"}`)
	req := httptest.NewRequest("PUT", "/api/file?path="+url.QueryEscape(path), body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleFile(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	got, _ := os.ReadFile(path)
	if string(got) != "new content" {
		t.Errorf("file content = %q, want 'new content'", string(got))
	}
}

func TestHandleFileWriteNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")

	body := strings.NewReader(`{"content":"created"}`)
	req := httptest.NewRequest("PUT", "/api/file?path="+url.QueryEscape(path), body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleFile(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	got, _ := os.ReadFile(path)
	if string(got) != "created" {
		t.Errorf("file content = %q, want 'created'", string(got))
	}
}

func TestHandleFileWriteInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("old"), 0644)

	body := strings.NewReader(`not json`)
	req := httptest.NewRequest("PUT", "/api/file?path="+url.QueryEscape(path), body)
	w := httptest.NewRecorder()
	handleFile(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleFileMethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest("DELETE", "/api/file?path=foo", nil)
	w := httptest.NewRecorder()
	handleFile(w, req)

	if w.Code != 405 {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

// --- handleStats ---

func TestHandleStatsReturnsJSON(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/stats", nil)
	w := httptest.NewRecorder()
	handleStats(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp statsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

// --- sorting ---

func TestFileEntrySorting(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "zebra.txt"), []byte{}, 0644)
	os.WriteFile(filepath.Join(dir, "alpha.txt"), []byte{}, 0644)
	os.Mkdir(filepath.Join(dir, "mydir"), 0755)
	os.WriteFile(filepath.Join(dir, "Beta.txt"), []byte{}, 0644)

	req := httptest.NewRequest("GET", "/api/files?path="+url.QueryEscape(dir), nil)
	w := httptest.NewRecorder()
	handleFiles(w, req)

	var resp filesResponse
	json.NewDecoder(w.Body).Decode(&resp)

	// Directory first
	if !resp.Entries[0].IsDir {
		t.Error("directory should come first")
	}

	// Files sorted case-insensitively
	names := make([]string, 0)
	for _, e := range resp.Entries {
		if !e.IsDir {
			names = append(names, e.Name)
		}
	}
	for i := 1; i < len(names); i++ {
		if strings.ToLower(names[i-1]) > strings.ToLower(names[i]) {
			t.Errorf("files not sorted: %v", names)
			break
		}
	}
}

// --- content type ---

func TestAPIResponsesHaveJSONContentType(t *testing.T) {
	handlers := []struct {
		name    string
		method  string
		path    string
		handler http.HandlerFunc
	}{
		{"files", "GET", "/api/files", handleFiles},
		{"file", "GET", "/api/file?path=nonexistent", handleFile},
		{"stats", "GET", "/api/stats", handleStats},
	}

	for _, h := range handlers {
		t.Run(h.name, func(t *testing.T) {
			req := httptest.NewRequest(h.method, h.path, nil)
			w := httptest.NewRecorder()
			h.handler(w, req)
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("%s Content-Type = %q, want application/json", h.name, ct)
			}
		})
	}
}
