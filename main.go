package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/UserExistsError/conpty"
	"github.com/gorilla/websocket"
)

//go:embed public
var staticFiles embed.FS

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type config struct {
	Port  string
	Shell string
}

func loadConfig() config {
	cfg := config{Port: "1122", Shell: "powershell.exe"}

	exePath, err := os.Executable()
	if err != nil {
		return cfg
	}
	f, err := os.Open(filepath.Join(filepath.Dir(exePath), "webterm.ini"))
	if err != nil {
		return cfg
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(strings.ToLower(k)) {
		case "port":
			cfg.Port = strings.TrimSpace(v)
		case "shell":
			cfg.Shell = strings.TrimSpace(v)
		}
	}
	return cfg
}

// --- File explorer API ---

type fileEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
	Git   string `json:"git,omitempty"`
}

type filesResponse struct {
	Path      string      `json:"path"`
	GitBranch string      `json:"gitBranch,omitempty"`
	Entries   []fileEntry `json:"entries"`
}

func handleFiles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir, _ = os.Getwd()
	}
	dir = filepath.Clean(dir)

	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	gitStatuses := getGitStatuses(dir)
	gitBranch := getGitBranch(dir)

	entries := make([]fileEntry, 0, len(dirEntries))
	for _, e := range dirEntries {
		var size int64
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		entries = append(entries, fileEntry{
			Name:  e.Name(),
			IsDir: e.IsDir(),
			Size:  size,
			Git:   gitStatuses[e.Name()],
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})

	json.NewEncoder(w).Encode(filesResponse{
		Path:      dir,
		GitBranch: gitBranch,
		Entries:   entries,
	})
}

func getGitBranch(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func getGitStatuses(dir string) map[string]string {
	rootBytes, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil
	}
	root := strings.TrimSpace(string(rootBytes))

	out, err := exec.Command("git", "-C", root, "status", "--porcelain", "-u").Output()
	if err != nil {
		return nil
	}

	relDir, err := filepath.Rel(root, dir)
	if err != nil {
		return nil
	}
	relDir = filepath.ToSlash(relDir)
	if relDir == "." {
		relDir = ""
	} else {
		relDir += "/"
	}

	statuses := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		status := strings.TrimSpace(line[:2])
		filePath := filepath.ToSlash(line[3:])

		if idx := strings.Index(filePath, " -> "); idx >= 0 {
			filePath = filePath[idx+4:]
		}

		if relDir != "" && !strings.HasPrefix(filePath, relDir) {
			continue
		}

		name := strings.SplitN(strings.TrimPrefix(filePath, relDir), "/", 2)[0]

		if _, exists := statuses[name]; !exists {
			statuses[name] = status
		}
	}
	return statuses
}

// --- Shell integration (OSC 7 CWD reporting) ---

func shellInit(shell string) string {
	base := strings.ToLower(filepath.Base(shell))
	switch {
	case strings.Contains(base, "powershell") || strings.Contains(base, "pwsh"):
		return "$function:__wt_op = $function:prompt\r\nfunction global:prompt { $e=[char]0x1b; $b=[char]7; [Console]::Write(\"${e}]7;$($PWD.Path)${b}\"); & $function:__wt_op }\r\ncls\r\n"
	case strings.Contains(base, "cmd"):
		return "prompt $E]7;$P$E\\$P$G\r\ncls\r\n"
	default:
		return ""
	}
}

// --- Terminal WebSocket ---

type resizeMsg struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

func handleWS(w http.ResponseWriter, r *http.Request, shell string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade error: %v", err)
		return
	}
	defer conn.Close()

	cpty, err := conpty.Start(shell, conpty.ConPtyDimensions(80, 24))
	if err != nil {
		log.Printf("conpty start error: %v", err)
		return
	}
	defer cpty.Close()

	// Inject shell integration for CWD tracking via OSC 7
	if init := shellInit(shell); init != "" {
		cpty.Write([]byte(init))
	}

	var wsMu sync.Mutex

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := cpty.Read(buf)
			if err != nil {
				conn.Close()
				return
			}
			wsMu.Lock()
			err = conn.WriteMessage(websocket.TextMessage, buf[:n])
			wsMu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if len(msg) > 0 && msg[0] == 0x01 {
			var rm resizeMsg
			if json.Unmarshal(msg[1:], &rm) == nil {
				cpty.Resize(rm.Cols, rm.Rows)
			}
			continue
		}
		cpty.Write(msg)
	}
}

// --- Main ---

func main() {
	cfg := loadConfig()

	publicFS, _ := fs.Sub(staticFiles, "public")
	http.Handle("/", http.FileServer(http.FS(publicFS)))
	http.HandleFunc("/api/files", handleFiles)
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleWS(w, r, cfg.Shell)
	})

	addr := fmt.Sprintf("0.0.0.0:%s", cfg.Port)
	log.Printf("webterm listening on http://%s/", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
