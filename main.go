package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	"net"
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

var (
	flagPort    = flag.String("port", "1122", "listen port")
	flagShell   = flag.String("shell", "powershell.exe", "shell executable")
	flagStats   = flag.Int("stats", 2000, "resource monitor refresh interval in milliseconds")
	flagTLS     = flag.Bool("tls", false, "enable TLS with auto-generated self-signed certificate")
	flagTLSCert = flag.String("tls-cert", "", "path to TLS certificate file")
	flagTLSKey  = flag.String("tls-key", "", "path to TLS private key file")
)

var tlsEnabled bool

const (
	maxFileSize            = 5 << 20 // 5 MB
	ptyBufSize             = 16384
	loginDelay             = 10 * time.Second
	sessionTTL             = 24 * time.Hour
	tokenLen               = 32
	ifTypeSoftwareLoopback = 24 // MIB_IF_TYPE_SOFTWARE_LOOPBACK
	ifOperConnected        = 4  // INTERNAL_IF_OPER_STATUS: connected (WAN)
	ifOperOperational      = 5  // INTERNAL_IF_OPER_STATUS: operational (LAN)
)

// writeError sends a JSON error response. Encoding failures are ignorable
// because the HTTP status code is already written at that point.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
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
		dir, _ = os.Getwd() // empty string is an acceptable fallback
	}
	dir = filepath.Clean(dir)

	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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

	out, err := exec.Command("git", "-C", root, "status", "--porcelain", "-u", "--ignored").Output()
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

	gitPriority := map[string]int{"M": 4, "A": 3, "D": 2, "R": 1, "??": 0, "!!": -1}
	prio := func(status string) int {
		if p, ok := gitPriority[status]; ok {
			return p
		}
		s := strings.TrimSpace(status)
		if len(s) > 0 {
			if p, ok := gitPriority[string(s[0])]; ok {
				return p
			}
		}
		return -1
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

		if prev, exists := statuses[name]; !exists || prio(status) > prio(prev) {
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
		writeError(w, http.StatusBadRequest, "path required")
		return
	}
	filePath = filepath.Clean(filePath)

	switch r.Method {
	case http.MethodGet:
		info, err := os.Stat(filePath)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if info.IsDir() {
			writeError(w, http.StatusBadRequest, "is a directory")
			return
		}
		if info.Size() > maxFileSize {
			writeError(w, http.StatusBadRequest, "file too large (max 5 MB)")
			return
		}
		content, err := os.ReadFile(filePath)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		json.NewEncoder(w).Encode(map[string]string{
			"path":    filePath,
			"content": string(content),
		})

	case http.MethodPut:
		var body struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := os.WriteFile(filePath, []byte(body.Content), 0644); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"ok": "true"})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// --- Shell integration (OSC 7 CWD reporting) ---

func shellInit(shell string) string {
	base := strings.ToLower(filepath.Base(shell))
	switch {
	case strings.Contains(base, "powershell") || strings.Contains(base, "pwsh"):
		return "$function:__wt_op = $function:prompt\r\n" +
			"function global:prompt { $e=[char]0x1b; $b=[char]7; [Console]::Write(\"${e}]7;$($PWD.Path)${b}\"); [Console]::Write(\"${e}]0;$($PWD.Path.Split('\\')[-1])${b}\"); & $function:__wt_op }\r\n" +
			"Set-PSReadLineKeyHandler -Key Enter -ScriptBlock { $l=''; $c=0; [Microsoft.PowerShell.PSConsoleReadLine]::GetBufferState([ref]$l,[ref]$c); if($l.Trim()){ $cmd=($l.Trim()-split '\\s+')[0]; $e=[char]0x1b; $b=[char]7; [Console]::Write(\"${e}]0;${cmd}${b}\") }; [Microsoft.PowerShell.PSConsoleReadLine]::AcceptLine() }\r\n" +
			"cls\r\n"
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

	// Inject shell integration for CWD tracking via OSC 7.
	// Write error is ignorable — shell works without integration.
	if init := shellInit(shell); init != "" {
		cpty.Write([]byte(init))
	}

	var wsMu sync.Mutex

	// Monitor process exit.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		exitCode, err := cpty.Wait(ctx)
		if err == nil {
			log.Printf("shell exited: code=%d", exitCode)
		}
	}()

	// ConPTY → WebSocket
	go func() {
		buf := make([]byte, ptyBufSize)
		for {
			n, err := cpty.Read(buf)
			if n > 0 {
				wsMu.Lock()
				werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n])
				wsMu.Unlock()
				if werr != nil {
					return
				}
			}
			if err != nil {
				log.Printf("pty read: %v", err)
				wsMu.Lock()
				// Best-effort close frame; connection is shutting down.
				conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "terminal exited"))
				wsMu.Unlock()
				conn.Close()
				return
			}
		}
	}()

	// WebSocket → ConPTY
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if len(msg) > 0 && msg[0] == 0x01 {
			var rm resizeMsg
			if json.Unmarshal(msg[1:], &rm) == nil {
				if err := cpty.Resize(rm.Cols, rm.Rows); err != nil {
					log.Printf("pty resize: %v", err)
				}
			}
			continue
		}
		if _, err := cpty.Write(msg); err != nil {
			log.Printf("pty write: %v", err)
			break
		}
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
	_               [4]byte // padding — Windows sizeof(MIB_IFROW)=860
}

type statsResponse struct {
	CPU      float64  `json:"cpu"`
	RAM      ramInfo  `json:"ram"`
	GPU      *gpuInfo `json:"gpu"`
	Net      netInfo  `json:"net"`
	CPUTemp  *float64 `json:"cpuTemp"`
	Interval int      `json:"interval"`
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

// cpuTimes returns cumulative idle and total CPU ticks via GetSystemTimes.
// On failure the syscall writes zero values, yielding 0% CPU — acceptable.
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

// ramStats returns physical memory usage via GlobalMemoryStatusEx.
// On failure the struct stays zeroed, yielding 0/0 — acceptable.
func ramStats() ramInfo {
	var ms memStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	return ramInfo{Used: ms.TotalPhys - ms.AvailPhys, Total: ms.TotalPhys}
}

func netTotals() (rx, tx uint64) {
	// First call with nil buffer queries the required size.
	var size uint32
	procGetIfTable.Call(0, uintptr(unsafe.Pointer(&size)), 0)
	if size == 0 {
		return 0, 0
	}
	buf := make([]byte, size)
	ret, _, _ := procGetIfTable.Call( // errno is redundant with ret
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
		0,
	)
	if ret != 0 {
		return 0, 0
	}
	n := *(*uint32)(unsafe.Pointer(&buf[0]))
	rowSz := int(unsafe.Sizeof(mibIfRow{}))
	for i := 0; i < int(n); i++ {
		off := 4 + i*rowSz
		row := (*mibIfRow)(unsafe.Pointer(&buf[off]))
		if (row.OperStatus != ifOperConnected && row.OperStatus != ifOperOperational) || row.Type == ifTypeSoftwareLoopback {
			continue
		}
		rx += uint64(row.InOctets)
		tx += uint64(row.OutOctets)
	}
	return rx, tx
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

// gpuStats queries nvidia-smi for GPU metrics. Parse errors default to
// zero values which is acceptable for a best-effort monitoring display.
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

// statsCollector polls system metrics in a loop. It runs for the lifetime
// of the process, started from main as a background goroutine.
func statsCollector(interval time.Duration) {
	prevIdle, prevTotal := cpuTimes()
	prevRx, prevTx := netTotals()
	prevTime := time.Now()
	hasGPU := true
	hasCPUTemp := true

	for {
		time.Sleep(interval)

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
		elapsed := now.Sub(prevTime).Seconds()
		var rxRate, txRate uint64
		if elapsed > 0 {
			if rx >= prevRx {
				rxRate = uint64(float64(rx-prevRx) / elapsed)
			}
			if tx >= prevTx {
				txRate = uint64(float64(tx-prevTx) / elapsed)
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

		var temp *float64
		if hasCPUTemp {
			if t, ok := cpuTemp(); ok {
				temp = &t
			} else {
				hasCPUTemp = false
			}
		}

		statsMu.Lock()
		latestStats = statsResponse{
			CPU:      cpu,
			RAM:      ram,
			GPU:      gpu,
			Net:      netInfo{Rx: rxRate, Tx: txRate},
			CPUTemp:  temp,
			Interval: *flagStats,
		}
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

// --- Authentication ---

var (
	modAdvapi32    = syscall.NewLazyDLL("advapi32.dll")
	procLogonUserW = modAdvapi32.NewProc("LogonUserW")
)

var (
	sessions   = make(map[string]time.Time)
	sessionsMu sync.Mutex
)

func validateLogin(username, password string) bool {
	domain := "."
	if i := strings.IndexByte(username, '\\'); i >= 0 {
		domain = username[:i]
		username = username[i+1:]
	}
	// UTF16PtrFromString only fails on embedded NUL which valid
	// credentials never contain.
	uUser, _ := syscall.UTF16PtrFromString(username)
	uDomain, _ := syscall.UTF16PtrFromString(domain)
	uPass, _ := syscall.UTF16PtrFromString(password)
	var token syscall.Handle
	ret, _, _ := procLogonUserW.Call(
		uintptr(unsafe.Pointer(uUser)),
		uintptr(unsafe.Pointer(uDomain)),
		uintptr(unsafe.Pointer(uPass)),
		3, // LOGON32_LOGON_NETWORK
		0, // LOGON32_PROVIDER_DEFAULT
		uintptr(unsafe.Pointer(&token)),
	)
	if ret != 0 {
		syscall.CloseHandle(token)
		return true
	}
	return false
}

func checkSession(r *http.Request) bool {
	cookie, err := r.Cookie("webterm_session")
	if err != nil {
		return false
	}
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	exp, ok := sessions[cookie.Value]
	if !ok || time.Now().After(exp) {
		delete(sessions, cookie.Value)
		return false
	}
	return true
}

func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkSession(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

func handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if checkSession(r) {
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}
	writeError(w, http.StatusUnauthorized, "unauthorized")
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if !validateLogin(body.Username, body.Password) {
		time.Sleep(loginDelay)
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	tokenRaw := make([]byte, tokenLen)
	rand.Read(tokenRaw) // crypto/rand.Read never returns error
	token := hex.EncodeToString(tokenRaw)
	sessionsMu.Lock()
	now := time.Now()
	for k, v := range sessions {
		if now.After(v) {
			delete(sessions, k)
		}
	}
	sessions[token] = now.Add(sessionTTL)
	sessionsMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     "webterm_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   tlsEnabled,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	log.Printf("login: %q", body.Username)
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// --- TLS ---

// tlsErrorLog returns a logger that silences routine TLS handshake errors
// (e.g. clients rejecting self-signed certs). These are expected and noisy.
func tlsErrorLog() *log.Logger {
	return log.New(&tlsHandshakeFilter{}, "", 0)
}

type tlsHandshakeFilter struct{}

func (f *tlsHandshakeFilter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "TLS handshake error") {
		return len(p), nil // swallow
	}
	return os.Stderr.Write(p)
}

func localIPs() []net.IP {
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok {
				ips = append(ips, ipNet.IP)
			}
		}
	}
	return ips
}

func generateSelfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	ips := localIPs()
	hostname, _ := os.Hostname() // empty hostname is acceptable
	dnsNames := []string{"localhost"}
	if hostname != "" {
		dnsNames = append(dnsNames, hostname)
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"webterm"}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return tls.X509KeyPair(certPEM, keyPEM)
}

// --- Main ---

func main() {
	flag.Parse()
	go statsCollector(time.Duration(*flagStats) * time.Millisecond)

	if (*flagTLSCert == "") != (*flagTLSKey == "") {
		log.Fatal("-tls-cert and -tls-key must be used together")
	}
	tlsEnabled = *flagTLS || *flagTLSCert != ""

	publicFS, _ := fs.Sub(staticFiles, "public") // embedded FS always contains "public"
	http.Handle("/", http.FileServer(http.FS(publicFS)))
	http.HandleFunc("/api/auth/check", handleAuthCheck)
	http.HandleFunc("/api/auth/login", handleLogin)
	http.HandleFunc("/api/files", requireAuth(handleFiles))
	http.HandleFunc("/api/file", requireAuth(handleFile))
	http.HandleFunc("/api/stats", requireAuth(handleStats))
	http.HandleFunc("/ws", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		handleWS(w, r, *flagShell)
	}))

	addr := fmt.Sprintf("0.0.0.0:%s", *flagPort)

	if tlsEnabled {
		if *flagTLSCert != "" && *flagTLSKey != "" {
			srv := &http.Server{
				Addr:         addr,
				TLSConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
				TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
				ErrorLog:     tlsErrorLog(),
			}
			log.Printf("webterm listening on https://%s/", addr)
			log.Fatal(srv.ListenAndServeTLS(*flagTLSCert, *flagTLSKey))
		} else {
			cert, err := generateSelfSignedCert()
			if err != nil {
				log.Fatalf("failed to generate self-signed certificate: %v", err)
			}
			// Cert was just created so parsing always succeeds.
			parsed, _ := x509.ParseCertificate(cert.Certificate[0])
			var sans []string
			for _, ip := range parsed.IPAddresses {
				sans = append(sans, ip.String())
			}
			log.Printf("self-signed cert SANs: DNS=%v IP=%v", parsed.DNSNames, sans)
			log.Printf("webterm listening on https://%s/ (self-signed certificate)", addr)
			srv := &http.Server{
				Addr: addr,
				TLSConfig: &tls.Config{
					Certificates: []tls.Certificate{cert},
					MinVersion:   tls.VersionTLS12,
				},
				TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
				ErrorLog:     tlsErrorLog(),
			}
			log.Fatal(srv.ListenAndServeTLS("", ""))
		}
	} else {
		log.Printf("webterm listening on http://%s/", addr)
		log.Fatal(http.ListenAndServe(addr, nil))
	}
}
