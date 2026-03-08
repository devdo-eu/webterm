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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

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

// --- File editor API ---

func handleFile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "path required"})
		return
	}
	filePath = filepath.Clean(filePath)

	switch r.Method {
	case "GET":
		info, err := os.Stat(filePath)
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if info.IsDir() {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "is a directory"})
			return
		}
		if info.Size() > 5<<20 {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "file too large (max 5 MB)"})
			return
		}
		content, err := os.ReadFile(filePath)
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"path": filePath, "content": string(content)})

	case "PUT":
		var body struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if err := os.WriteFile(filePath, []byte(body.Content), 0644); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"ok": "true"})

	default:
		w.WriteHeader(405)
		json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
	}
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

// --- Resource monitor ---

var (
	modKernel32              = syscall.NewLazyDLL("kernel32.dll")
	modIphlpapi              = syscall.NewLazyDLL("iphlpapi.dll")
	procGetSystemTimes       = modKernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = modKernel32.NewProc("GlobalMemoryStatusEx")
	procGetIfTable           = modIphlpapi.NewProc("GetIfTable")
)

type memStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

type mibIfRow struct {
	Name            [256]uint16
	Index           uint32
	Type            uint32
	Mtu             uint32
	Speed           uint32
	PhysAddrLen     uint32
	PhysAddr        [8]byte
	AdminStatus     uint32
	OperStatus      uint32
	LastChange      uint32
	InOctets        uint32
	InUcastPkts     uint32
	InNUcastPkts    uint32
	InDiscards      uint32
	InErrors        uint32
	InUnknownProtos uint32
	OutOctets       uint32
	OutUcastPkts    uint32
	OutNUcastPkts   uint32
	OutDiscards     uint32
	OutErrors       uint32
	DescrLen        uint32
	Descr           [256]byte
}

type statsResponse struct {
	CPU     float64  `json:"cpu"`
	RAM     ramInfo  `json:"ram"`
	GPU     *gpuInfo `json:"gpu"`
	Net     netInfo  `json:"net"`
	CPUTemp *float64 `json:"cpuTemp"`
}

type ramInfo struct {
	Used  uint64 `json:"used"`
	Total uint64 `json:"total"`
}

type gpuInfo struct {
	Usage    int     `json:"usage"`
	MemUsed  int     `json:"memUsed"`
	MemTotal int     `json:"memTotal"`
	Name     string  `json:"name"`
	Power    float64 `json:"power"`
	Temp     int     `json:"temp"`
}

type netInfo struct {
	Rx uint64 `json:"rx"`
	Tx uint64 `json:"tx"`
}

var (
	statsMu     sync.Mutex
	latestStats statsResponse
)

func cpuTimes() (idle, total uint64) {
	var idleFt, kernelFt, userFt [2]uint32
	procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idleFt)),
		uintptr(unsafe.Pointer(&kernelFt)),
		uintptr(unsafe.Pointer(&userFt)),
	)
	idle = uint64(idleFt[1])<<32 | uint64(idleFt[0])
	total = (uint64(kernelFt[1])<<32 | uint64(kernelFt[0])) +
		(uint64(userFt[1])<<32 | uint64(userFt[0]))
	return
}

func ramStats() ramInfo {
	var ms memStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	return ramInfo{Used: ms.TotalPhys - ms.AvailPhys, Total: ms.TotalPhys}
}

func netTotals() (rx, tx uint64) {
	var size uint32
	procGetIfTable.Call(0, uintptr(unsafe.Pointer(&size)), 0)
	if size == 0 {
		return
	}
	buf := make([]byte, size)
	ret, _, _ := procGetIfTable.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
		0,
	)
	if ret != 0 {
		return
	}
	n := *(*uint32)(unsafe.Pointer(&buf[0]))
	rowSz := int(unsafe.Sizeof(mibIfRow{}))
	for i := 0; i < int(n); i++ {
		off := 4 + i*rowSz
		row := (*mibIfRow)(unsafe.Pointer(&buf[off]))
		if row.OperStatus != 1 || row.Type == 24 {
			continue
		}
		rx += uint64(row.InOctets)
		tx += uint64(row.OutOctets)
	}
	return
}

func cpuTemp() (float64, bool) {
	out, err := exec.Command("wmic", "/namespace:\\\\root\\wmi", "PATH",
		"MSAcpi_ThermalZoneTemperature", "get", "CurrentTemperature", "/value").Output()
	if err != nil {
		return 0, false
	}
	var maxTemp float64
	found := false
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "CurrentTemperature=") {
			valStr := strings.TrimPrefix(line, "CurrentTemperature=")
			val, err := strconv.Atoi(strings.TrimSpace(valStr))
			if err == nil && val > 0 {
				c := float64(val)/10.0 - 273.15
				if c > maxTemp {
					maxTemp = c
				}
				found = true
			}
		}
	}
	return maxTemp, found
}

func gpuStats() *gpuInfo {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=utilization.gpu,memory.used,memory.total,name,power.draw,temperature.gpu",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	parts := strings.Split(strings.TrimSpace(string(out)), ", ")
	if len(parts) < 6 {
		return nil
	}
	usage, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
	memUsed, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
	memTotal, _ := strconv.Atoi(strings.TrimSpace(parts[2]))
	power, _ := strconv.ParseFloat(strings.TrimSpace(parts[4]), 64)
	temp, _ := strconv.Atoi(strings.TrimSpace(parts[5]))
	return &gpuInfo{
		Usage: usage, MemUsed: memUsed, MemTotal: memTotal,
		Name: strings.TrimSpace(parts[3]), Power: power, Temp: temp,
	}
}

func statsCollector() {
	prevIdle, prevTotal := cpuTimes()
	prevRx, prevTx := netTotals()
	prevTime := time.Now()
	hasGPU := true
	hasCPUTemp := true

	for {
		time.Sleep(1 * time.Second)

		idle, total := cpuTimes()
		dIdle, dTotal := idle-prevIdle, total-prevTotal
		var cpu float64
		if dTotal > 0 {
			cpu = (1 - float64(dIdle)/float64(dTotal)) * 100
		}
		prevIdle, prevTotal = idle, total

		ram := ramStats()

		rx, tx := netTotals()
		now := time.Now()
		dt := now.Sub(prevTime).Seconds()
		var nRx, nTx uint64
		if dt > 0 {
			if rx >= prevRx {
				nRx = uint64(float64(rx-prevRx) / dt)
			}
			if tx >= prevTx {
				nTx = uint64(float64(tx-prevTx) / dt)
			}
		}
		prevRx, prevTx, prevTime = rx, tx, now

		var gpu *gpuInfo
		if hasGPU {
			gpu = gpuStats()
			if gpu == nil {
				hasGPU = false
			}
		}

		var ct *float64
		if hasCPUTemp {
			if t, ok := cpuTemp(); ok {
				ct = &t
			} else {
				hasCPUTemp = false
			}
		}

		statsMu.Lock()
		latestStats = statsResponse{CPU: cpu, RAM: ram, GPU: gpu, Net: netInfo{Rx: nRx, Tx: nTx}, CPUTemp: ct}
		statsMu.Unlock()
	}
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	statsMu.Lock()
	s := latestStats
	statsMu.Unlock()
	json.NewEncoder(w).Encode(s)
}

// --- Main ---

func main() {
	cfg := loadConfig()
	go statsCollector()

	publicFS, _ := fs.Sub(staticFiles, "public")
	http.Handle("/", http.FileServer(http.FS(publicFS)))
	http.HandleFunc("/api/files", handleFiles)
	http.HandleFunc("/api/file", handleFile)
	http.HandleFunc("/api/stats", handleStats)
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleWS(w, r, cfg.Shell)
	})

	addr := fmt.Sprintf("0.0.0.0:%s", cfg.Port)
	log.Printf("webterm listening on http://%s/", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
