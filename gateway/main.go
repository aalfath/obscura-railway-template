// obscura-gateway: the public front door for Obscura on Railway.
//
// MCP runs on demand (see mcpManager) and every browser session takes one
// memory slot, so a small service refuses extra sessions instead of being
// killed for running out of memory.
//
// It runs as PID 1 and
//   - starts `obscura serve` (CDP) on 127.0.0.1:9222 and `obscura mcp --http`
//     on 127.0.0.1:3000 as the unprivileged user 65532, each with a random
//     internal token generated at boot;
//   - listens on $PORT and accepts only requests that carry OBSCURA_TOKEN,
//     either as "Authorization: Bearer <token>" or as "?token=<token>";
//   - proxies CDP discovery (/json/*) and WebSockets to Obscura, rewriting
//     the discovery URLs to this service's public wss:// address;
//   - proxies /mcp to Obscura's streamable-HTTP MCP server;
//   - answers /healthz without a token;
//   - exits non-zero as soon as either Obscura process exits, so Railway's
//     ON_FAILURE restart policy brings the service back.
// Standard library only.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	cdpAddr  = "127.0.0.1:9222"
	mcpAddr  = "127.0.0.1:3000"
	appUID   = 65532
	dataRoot = "/data"
)

var (
	publicToken string
	cdpToken    string
	mcpToken    string
	version     = "dev"
	shuttingDown atomic.Bool
	sessionSlots chan struct{}
	mcpIdle      = 180 * time.Second
	mcpMgr       *mcpManager
)

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func busy(w http.ResponseWriter, why string) {
	w.Header().Set("Retry-After", "30")
	http.Error(w, fmt.Sprintf("obscura is busy: %s (%d at a time with this service's memory). Close a session, wait for the MCP server to go idle (%v), or give the service more memory.", why, cap(sessionSlots), mcpIdle), http.StatusServiceUnavailable)
}

// mcpManager runs `obscura mcp --http` on demand. While it runs it holds one
// session slot; after mcpIdle without requests it is stopped (SIGTERM, so the
// profile is flushed) and the slot and its memory are freed.
type mcpManager struct {
	mu       sync.Mutex
	args     []string
	cmd      *exec.Cmd
	inflight int
	lastUse  time.Time
	stopping bool
	exited   chan string
}

func (m *mcpManager) running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cmd != nil && !m.stopping
}

func (m *mcpManager) begin() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == nil || m.stopping {
		if m.stopping {
			return fmt.Errorf("the MCP server is restarting, retry in a few seconds")
		}
		select {
		case sessionSlots <- struct{}{}:
		default:
			return fmt.Errorf("all browser sessions are in use, so the MCP server can't start")
		}
		cmd := child("mcp", m.args, []string{"OBSCURA_MCP_TOKEN=" + mcpToken, "OBSCURA_STORAGE_DIR=" + dataRoot + "/mcp-profile"})
		if err := cmd.Start(); err != nil {
			<-sessionSlots
			return fmt.Errorf("start MCP server: %v", err)
		}
		m.cmd = cmd
		go m.wait(cmd)
		deadline := time.Now().Add(20 * time.Second)
		for mcpHealthy() != nil {
			if time.Now().After(deadline) {
				return fmt.Errorf("the MCP server did not start in time")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	m.inflight++
	m.lastUse = time.Now()
	return nil
}

func (m *mcpManager) end() {
	m.mu.Lock()
	m.inflight--
	m.lastUse = time.Now()
	m.mu.Unlock()
}

func (m *mcpManager) wait(cmd *exec.Cmd) {
	err := cmd.Wait()
	m.mu.Lock()
	intended := m.stopping && m.cmd == cmd
	if m.cmd == cmd {
		m.cmd = nil
		m.stopping = false
		<-sessionSlots
	}
	m.mu.Unlock()
	if intended {
		log.Printf("MCP server stopped after %v idle (memory released)", mcpIdle)
		return
	}
	if !shuttingDown.Load() {
		m.exited <- fmt.Sprintf("obscura mcp exited: %v", err)
	}
}

func (m *mcpManager) idleLoop() {
	for range time.Tick(5 * time.Second) {
		m.mu.Lock()
		if m.cmd != nil && !m.stopping && m.inflight == 0 && time.Since(m.lastUse) > mcpIdle {
			m.stopping = true
			syscall.Kill(-m.cmd.Process.Pid, syscall.SIGTERM)
			go func(c *exec.Cmd) {
				time.Sleep(10 * time.Second)
				m.mu.Lock()
				if m.cmd == c {
					syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
				}
				m.mu.Unlock()
			}(m.cmd)
		}
		m.mu.Unlock()
	}
}

func (m *mcpManager) signal(sig syscall.Signal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil && m.cmd.Process != nil {
		syscall.Kill(-m.cmd.Process.Pid, sig)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// memoryLimitMB reads the cgroup v2 (or v1) memory limit. 0 means unknown.
func memoryLimitMB() int {
	for _, p := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		if s == "max" {
			return 0
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 || n > 1<<50 {
			return 0
		}
		return int(n / (1024 * 1024))
	}
	return 0
}

// tokenFrom returns the token a request carries, and whether it came from the query string.
func tokenFrom(r *http.Request) (string, bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")), false
	}
	if t := r.URL.Query().Get("token"); t != "" {
		return t, true
	}
	return "", false
}

func authorized(r *http.Request) (bool, bool) {
	t, fromQuery := tokenFrom(r)
	if t == "" {
		return false, false
	}
	return subtle.ConstantTimeCompare([]byte(t), []byte(publicToken)) == 1, fromQuery
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="obscura"`)
	http.Error(w, "unauthorized: send OBSCURA_TOKEN as 'Authorization: Bearer <token>' or '?token=<token>'", http.StatusUnauthorized)
}

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// publicBase returns ws(s)://host as the client sees this service.
func publicBase(r *http.Request) string {
	scheme := "ws"
	proto := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]))
	if proto == "https" || r.TLS != nil {
		scheme = "wss"
	}
	host := r.Host
	if xf := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); xf != "" {
		host = xf
	}
	return scheme + "://" + host
}

// stripToken removes the public token from the query before the request goes upstream.
func stripToken(u *url.URL) {
	q := u.Query()
	if q.Has("token") {
		q.Del("token")
		u.RawQuery = q.Encode()
	}
}

func newProxy(addr, internalToken string, rewriteDiscovery bool) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "http", Host: addr}
	p := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = addr // Obscura checks Host against its loopback bind
			pr.Out.Header.Set("Authorization", "Bearer "+internalToken)
			// The gateway already authenticated the caller with OBSCURA_TOKEN;
			// browser Origin checks upstream would only reject legitimate
			// browser-based CDP/MCP clients.
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Del("X-Forwarded-Proto")
			stripToken(pr.Out.URL)
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxy %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "obscura is not reachable", http.StatusBadGateway)
		},
	}
	if rewriteDiscovery {
		p.ModifyResponse = func(resp *http.Response) error {
			if !strings.HasPrefix(resp.Request.URL.Path, "/json") || resp.StatusCode != http.StatusOK {
				return nil
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if err != nil {
				return err
			}
			r := resp.Request
			base, _ := r.Context().Value(ctxBase{}).(string)
			suffix, _ := r.Context().Value(ctxSuffix{}).(string)
			body = rewriteWS(body, base, suffix)
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
			return nil
		}
	}
	return p
}

type ctxBase struct{}
type ctxSuffix struct{}

// rewriteWS points every webSocketDebuggerUrl at the public address.
func rewriteWS(body []byte, base, suffix string) []byte {
	var v any
	if err := json.Unmarshal(body, &v); err != nil || base == "" {
		return body
	}
	fix := func(m map[string]any) {
		if s, ok := m["webSocketDebuggerUrl"].(string); ok {
			if u, err := url.Parse(s); err == nil {
				m["webSocketDebuggerUrl"] = base + u.Path + suffix
			}
		}
	}
	switch t := v.(type) {
	case map[string]any:
		fix(t)
	case []any:
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				fix(m)
			}
		}
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return body
	}
	return out
}

var healthClient = &http.Client{Timeout: 3 * time.Second}

func cdpHealthy() error {
	req, _ := http.NewRequest(http.MethodGet, "http://"+cdpAddr+"/json/version", nil)
	req.Header.Set("Authorization", "Bearer "+cdpToken)
	resp, err := healthClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cdp /json/version answered %d", resp.StatusCode)
	}
	return nil
}

func mcpHealthy() error {
	c, err := net.DialTimeout("tcp", mcpAddr, 2*time.Second)
	if err != nil {
		return err
	}
	return c.Close()
}

func prepareDir(path string) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		log.Fatalf("create %s: %v (is the volume mounted at %s?)", path, err, dataRoot)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(path, appUID, appUID); err != nil {
			log.Fatalf("chown %s: %v", path, err)
		}
		os.Chmod(path, 0o700)
	}
}

func child(name string, args []string, extraEnv []string) *exec.Cmd {
	cmd := exec.Command("/obscura", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	env := []string{}
	for _, kv := range os.Environ() {
		k := strings.SplitN(kv, "=", 2)[0]
		// Never hand the public token or Railway's own settings to the browser.
		if k == "OBSCURA_TOKEN" || k == "OBSCURA_CDP_TOKEN" || k == "OBSCURA_MCP_TOKEN" ||
			k == "BROWSER_WS_ENDPOINT" || k == "BROWSER_WS_ENDPOINT_PRIVATE" || k == "MCP_URL" {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, extraEnv...)
	cmd.Env = append(cmd.Env, "HOME=/home/nonroot", "TMPDIR=/tmp")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if os.Geteuid() == 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: appUID, Gid: appUID}
	}
	log.Printf("starting %s: obscura %s", name, strings.Join(args, " "))
	return cmd
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("[gateway] ")

	// Railway hosts run transparent huge pages "always"; V8 and glibc then
	// keep 2 MB pages for small heaps. Opt this process tree out (inherited by
	// the Obscura children). PR_SET_THP_DISABLE = 41.
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, 41, 1, 0, 0, 0, 0); e != 0 {
		log.Printf("prctl(PR_SET_THP_DISABLE) failed: %v (continuing)", e)
	}

	publicToken = os.Getenv("OBSCURA_TOKEN")
	if len(publicToken) < 32 {
		log.Printf("FATAL: OBSCURA_TOKEN must be set to at least 32 characters (the template generates one)")
		os.Exit(2)
	}
	cdpToken = randomHex(32)
	mcpToken = randomHex(32)
	port := envOr("PORT", "8080")

	// Size Obscura to the service's memory limit. A heavy page (GitHub, with
	// a screenshot) costs about 250 MB in one Obscura process, so every live
	// browser session (a CDP WebSocket, or the MCP server while in use) takes
	// one slot of about 250 MB above a ~120 MB base. More sessions than slots
	// get a clear 503 instead of an out-of-memory kill.
	limit := memoryLimitMB()
	heapMB, slots := 1024, 16
	if limit > 0 {
		heapMB = clamp(limit*2/5, 128, 2048)
		slots = clamp((limit-120)/250, 1, 64)
	}
	if v := os.Getenv("OBSCURA_MAX_SESSIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			slots = n
		}
	}
	sessionSlots = make(chan struct{}, slots)
	if v := os.Getenv("OBSCURA_MCP_IDLE_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			mcpIdle = time.Duration(n) * time.Second
		}
	}
	v8flags := envOr("OBSCURA_V8_FLAGS", fmt.Sprintf("--max-old-space-size=%d", heapMB))
	log.Printf("obscura-gateway %s: memory limit %d MB -> %d browser session(s) at a time, V8 old space %d MB per isolate, MCP stops after %v idle", version, limit, slots, heapMB, mcpIdle)

	prepareDir(dataRoot + "/cdp-profile")
	prepareDir(dataRoot + "/mcp-profile")

	global := []string{"--v8-flags", v8flags}
	if truthy(os.Getenv("OBSCURA_STEALTH")) {
		global = append(global, "--stealth")
	}
	if p := strings.TrimSpace(os.Getenv("OBSCURA_PROXY")); p != "" {
		global = append(global, "--proxy", p)
	}

	serveArgs := append(append([]string{}, global...), "serve", "--host", "127.0.0.1", "--port", "9222",
		"--storage-dir", dataRoot+"/cdp-profile", "--max-connections", strconv.Itoa(slots))
	mcpArgs := append(append([]string{}, global...), "mcp", "--http", "--host", "127.0.0.1", "--port", "3000")

	cdp := child("cdp", serveArgs, []string{"OBSCURA_CDP_TOKEN=" + cdpToken})
	if err := cdp.Start(); err != nil {
		log.Printf("FATAL: start obscura serve: %v", err)
		os.Exit(1)
	}
	exited := make(chan string, 4)
	go func() { err := cdp.Wait(); exited <- fmt.Sprintf("obscura serve exited: %v", err) }()
	mcpMgr = &mcpManager{args: mcpArgs, exited: exited}
	go mcpMgr.idleLoop()

	cdpProxy := newProxy(cdpAddr, cdpToken, true)
	mcpProxy := newProxy(mcpAddr, mcpToken, false)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := cdpHealthy(); err != nil {
			http.Error(w, "cdp: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			mcpProxy.ServeHTTP(w, r)
			return
		}
		if ok, _ := authorized(r); !ok {
			unauthorized(w)
			return
		}
		if r.Method != http.MethodPost {
			// Obscura's optional GET event stream only carries keep-alives.
			// Serve it while the MCP server runs; never start it for a GET.
			if r.Method == http.MethodGet && mcpMgr.running() {
				mcpProxy.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Allow", "POST, OPTIONS")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := mcpMgr.begin(); err != nil {
			busy(w, err.Error())
			return
		}
		defer mcpMgr.end()
		mcpProxy.ServeHTTP(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		isJSON := strings.HasPrefix(r.URL.Path, "/json")
		if !isJSON && !isWebSocket(r) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "Obscura %s is running.\n\nCDP (Puppeteer/Playwright): %s/devtools/browser?token=YOUR-OBSCURA-TOKEN\nMCP (streamable HTTP):      %s/mcp  with header  Authorization: Bearer YOUR-OBSCURA-TOKEN\n\nThe token is the OBSCURA_TOKEN variable of this Railway service.\n",
				version, publicBase(r), strings.Replace(strings.Replace(publicBase(r), "wss://", "https://", 1), "ws://", "http://", 1))
			return
		}
		ok, fromQuery := authorized(r)
		if !ok {
			unauthorized(w)
			return
		}
		if isJSON {
			suffix := ""
			if fromQuery {
				suffix = "?token=" + url.QueryEscape(publicToken)
			}
			ctx := context.WithValue(r.Context(), ctxBase{}, publicBase(r))
			ctx = context.WithValue(ctx, ctxSuffix{}, suffix)
			r = r.WithContext(ctx)
			r.Header.Del("Accept-Encoding")
		}
		if isWebSocket(r) {
			select {
			case sessionSlots <- struct{}{}:
				defer func() { <-sessionSlots }()
			default:
				busy(w, "all browser sessions are in use")
				return
			}
		}
		cdpProxy.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Printf("FATAL: listen :%s: %v", port, err)
		cdp.Process.Kill()
		os.Exit(1)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			exited <- "http server: " + err.Error()
		}
	}()
	if truthy(os.Getenv("OBSCURA_GATEWAY_STATS")) {
		go logStats()
	}
	log.Printf("listening on :%s (CDP at /devtools/browser and /json/*, MCP at /mcp, health at /healthz)", port)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	stop := func() {
		syscall.Kill(-cdp.Process.Pid, syscall.SIGTERM)
		mcpMgr.signal(syscall.SIGTERM)
	}
	select {
	case s := <-sigs:
		shuttingDown.Store(true)
		log.Printf("received %v, stopping Obscura", s)
		stop()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		srv.Shutdown(ctx)
		cancel()
		select {
		case <-exited:
		case <-time.After(8 * time.Second):
		}
		time.Sleep(time.Second)
		os.Exit(0)
	case why := <-exited:
		// Any unexpected exit, even with status 0, is a failure: exit non-zero
		// so Railway restarts the service.
		log.Printf("FATAL: %s; stopping so Railway restarts the service", why)
		stop()
		time.Sleep(2 * time.Second)
		syscall.Kill(-cdp.Process.Pid, syscall.SIGKILL)
		mcpMgr.signal(syscall.SIGKILL)
		os.Exit(1)
	}
}

// logStats prints cgroup memory figures every 30 s (debugging aid, off by default).
func logStats() {
	read := func(p string) string { b, _ := os.ReadFile(p); return strings.TrimSpace(string(b)) }
	field := func(text, key string) string {
		for _, l := range strings.Split(text, "\n") {
			if f := strings.Fields(strings.Replace(l, ":", " ", 1)); len(f) >= 2 && f[0] == key {
				return f[1]
			}
		}
		return "?"
	}
	for {
		stat := read("/sys/fs/cgroup/memory.stat")
		ev := read("/sys/fs/cgroup/memory.events")
		threads := 0
		procs := []string{}
		if ents, err := os.ReadDir("/proc"); err == nil {
			for _, e := range ents {
				if _, err := strconv.Atoi(e.Name()); err == nil {
					st := read("/proc/" + e.Name() + "/status")
					n, _ := strconv.Atoi(field(st, "Threads"))
					threads += n
					cmd := strings.Fields(strings.ReplaceAll(read("/proc/"+e.Name()+"/cmdline"), "\x00", " "))
					name := field(st, "Name")
					for _, a := range cmd {
						if a == "serve" || a == "mcp" {
							name = a
						}
					}
					procs = append(procs, fmt.Sprintf("%s:%skB/%sthr", name, field(st, "VmRSS"), field(st, "Threads")))
				}
			}
		}
		log.Printf("stats: memory.current=%s peak=%s anon=%s anon_thp=%s file=%s oom_kill=%s threads=%d %s",
			read("/sys/fs/cgroup/memory.current"), read("/sys/fs/cgroup/memory.peak"),
			field(stat, "anon"), field(stat, "anon_thp"), field(stat, "file"), field(ev, "oom_kill"), threads, strings.Join(procs, " "))
		time.Sleep(15 * time.Second)
	}
}
