package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alfg/ffmpegd/ffmpeg"
	"github.com/gorilla/websocket"
)

const (
	logo = `
███████╗███████╗███╗   ███╗██████╗ ███████╗ ██████╗ ██████╗
██╔════╝██╔════╝████╗ ████║██╔══██╗██╔════╝██╔════╝ ██╔══██╗
█████╗  █████╗  ██╔████╔██║██████╔╝█████╗  ██║  ███╗██║  ██║
██╔══╝  ██╔══╝  ██║╚██╔╝██║██╔═══╝ ██╔══╝  ██║   ██║██║  ██║
██║     ██║     ██║ ╚═╝ ██║██║     ███████╗╚██████╔╝██████╔╝
╚═╝     ╚═╝     ╚═╝     ╚═╝╚═╝     ╚══════╝ ╚═════╝ ╚═════╝
                                                      v2.1.0
	`
	version     = "ffmpegd version 2.1.0"
	description = "[\u001b[32mffmpegd\u001b[0m] - websocket server for \u001b[33mffmpeg-commander\u001b[0m.\n"
	usage       = `
Usage:
  ffmpegd [--host address] [port]   Run server on localhost:8080 by default.
  ffmpegd version                   Print version.
  ffmpegd help                      This help text.

Options:
  --host address   Address to listen on. Use 0.0.0.0 to let other machines on
                   your network connect. Can also be set with $FFMPEGD_HOST.
	`
	progressInterval = time.Second * 1
	writeTimeout     = time.Second * 10
	maxQueuedJobs    = 100
	maxRetainedTasks = 1000
)

var (
	port = "8080"
	host = "localhost"

	// Sites allowed to connect, besides the daemon's own localhost origin.
	remoteOrigins = []string{
		"https://alfg.github.io",
		"https://alfg.dev",
		"https://ffmpeg-commander.com",
		"https://www.ffmpeg-commander.com",
	}
	allowedOrigins []string

	mgr *manager

	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return isAllowedOrigin(r.Header.Get("Origin"))
		},
	}
)

// Message payload from client.
//
// ID is the stable task identifier introduced with task tracking: clients that
// send it receive the same ID on every event for the encode. Clients that omit
// it keep the original, ID-less broadcast behaviour.
//
// ClientID identifies the browser across reconnects, either via the
// ?client_id= query parameter or a hello message.
type Message struct {
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	ClientID string `json:"client_id,omitempty"`
	Input    string `json:"input"`
	Output   string `json:"output"`
	Payload  string `json:"payload"`
}

// Status is the legacy response shape, still sent to ID-less clients.
type Status struct {
	Percent   float64 `json:"percent"`
	Speed     string  `json:"speed"`
	FPS       float64 `json:"fps"`
	Err       string  `json:"err,omitempty"`
	Cancelled bool    `json:"cancelled,omitempty"`
}

// FilesResponse http response for files endpoint.
type FilesResponse struct {
	Cwd     string   `json:"cwd"`
	Folders []string `json:"folders"`
	Files   []file   `json:"files"`
}

type file struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func Run() {
	if err := parseArgs(os.Args[1:]); err != nil {
		fmt.Println("\u001b[31m" + err.Error() + "\u001b[0m")
		fmt.Println(usage)
		os.Exit(2)
	}
	allowedOrigins = append([]string{
		"http://localhost:" + port,
		"http://127.0.0.1:" + port,
	}, remoteOrigins...)

	// CLI Banner.
	printBanner()

	// Check if FFmpeg/FFprobe are available.
	err := verifyFFmpeg()
	if err != nil {
		fmt.Println("\u001b[31m" + err.Error() + "\u001b[0m")
		fmt.Println("\u001b[31mPlease ensure FFmpeg and FFprobe are installed and available on $PATH.\u001b[0m")
		os.Exit(1)
	}

	// HTTP/WS Server.
	startServer()
}

func parseArgs(args []string) error {
	if h := os.Getenv("FFMPEGD_HOST"); h != "" {
		host = h
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "version" || arg == "-v" || arg == "--version":
			fmt.Println(version)
			os.Exit(0)
		case arg == "help" || arg == "-h" || arg == "--help":
			fmt.Println(usage)
			os.Exit(0)
		case arg == "--host" || arg == "-host":
			if i+1 == len(args) {
				return errors.New("--host needs an address, such as 0.0.0.0")
			}
			i++
			host = args[i]
		case strings.HasPrefix(arg, "--host="):
			host = strings.TrimPrefix(arg, "--host=")
		default:
			if n, err := strconv.Atoi(arg); err != nil || n < 1 || n > 65535 {
				return fmt.Errorf("unknown argument %q", arg)
			}
			port = arg
		}
	}
	return nil
}

func printBanner() {
	fmt.Println(logo)
	fmt.Print(description + "\n")
}

// listen opens the server's listeners. "localhost" listens on both loopback
// addresses, since browsers may resolve it to either.
func listen() ([]net.Listener, error) {
	if host != "localhost" {
		l, err := net.Listen("tcp", net.JoinHostPort(host, port))
		if err != nil {
			return nil, err
		}
		return []net.Listener{l}, nil
	}

	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return nil, err
	}
	listeners := []net.Listener{l}
	// IPv6 may be unavailable; IPv4 alone is enough.
	if l6, err := net.Listen("tcp", net.JoinHostPort("::1", port)); err == nil {
		listeners = append(listeners, l6)
	}
	return listeners, nil
}

func isLoopback(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func startServer() {
	http.HandleFunc("/ws", handleConnections)
	http.HandleFunc("/files", handleFiles)
	http.Handle("/", http.FileServer(http.Dir("./")))

	mgr = newManager(newHub(), newRegistry(maxRetainedTasks))
	mgr.start()

	listeners, err := listen()
	if err != nil {
		fmt.Println("\u001b[31mCould not start server: " + err.Error() + "\u001b[0m")
		if strings.Contains(err.Error(), "address already in use") {
			fmt.Println("Is ffmpegd already running? To use another port, run: ffmpegd 8081")
		}
		os.Exit(1)
	}

	fmt.Println("  Server started on \u001b[33mhttp://" + net.JoinHostPort(host, port) + "\u001b[0m.")
	if !isLoopback(host) {
		fmt.Println("  \u001b[31mListening on " + host + ", so other machines may be able to connect.\u001b[0m")
	}
	fmt.Println("  - Go to \u001b[33mhttps://ffmpeg-commander.com\u001b[0m to connect!")
	fmt.Println("  - \u001b[33mffmpegd\u001b[0m must be enabled in ffmpeg-commander options.")
	fmt.Println("")
	fmt.Printf("Waiting for connection...")

	errs := make(chan error, len(listeners))
	for _, l := range listeners {
		go func(l net.Listener) { errs <- http.Serve(l, nil) }(l)
	}
	fmt.Println("\n\u001b[31mServer stopped: " + (<-errs).Error() + "\u001b[0m")
	os.Exit(1)
}

func isAllowedOrigin(origin string) bool {
	for _, o := range allowedOrigins {
		if origin == o {
			return true
		}
	}
	return false
}

// handleConnections owns the read loop of one websocket. Identity comes from
// the ?client_id= query parameter when the browser has one; otherwise a
// server-side ID is assigned and can later be claimed with a hello message.
func handleConnections(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); !isAllowedOrigin(origin) {
		fmt.Printf("\rRejected connection from %q: not an allowed origin.%s\n", origin, clearLine)
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Printf("\rWaiting for connection...\u001b[31m websocket connection failed!\u001b[0m")
		return
	}

	clientID := r.URL.Query().Get("client_id")
	identified := validClientID(clientID)
	if !identified {
		clientID = newClientID()
	}
	c := newClient(ws, clientID, identified)
	if identified {
		// A browser presenting a stored identity speaks the typed event
		// protocol, so enable it before sending welcome/tasks.
		c.enableTaskMode()
	}
	mgr.hub.add(c)
	defer mgr.disconnect(c)

	if identified {
		// A browser returning with a stored identity: confirm it and replay
		// the latest state of everything it owns, including terminal results
		// it may have missed while disconnected.
		c.send(Event{Type: "welcome", ClientID: clientID})
		mgr.tasks(c)
	}

	fmt.Printf("\rWaiting for connection......\u001b[32mconnected!\u001b[0m")
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			fmt.Printf("\rWaiting for connection...\u001b[31mdisconnected!\u001b[0m")
			return
		}

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			replyError(c, "", "invalid message: "+err.Error())
			continue
		}

		// Any ID-carrying or task-aware message switches the connection to
		// the typed event protocol for its replies.
		if msg.ID != "" || msg.Type == "hello" || msg.Type == "tasks" || msg.Type == "status" {
			c.enableTaskMode()
		}

		switch msg.Type {
		case "encode":
			if err := mgr.enqueue(c, msg); err != nil {
				replyError(c, msg.ID, err.Error())
			}
		case "cancel":
			if err := mgr.cancel(c, msg.ID); err != nil {
				replyError(c, msg.ID, err.Error())
			}
		case "hello":
			if err := mgr.hello(c, msg.ClientID); err != nil {
				replyError(c, "", err.Error())
			}
		case "status":
			mgr.status(c, msg.ID)
		case "tasks":
			mgr.tasks(c)
		default:
			replyError(c, "", fmt.Sprintf("unknown message type %q", msg.Type))
		}
	}
}

// replyError answers in whichever protocol the connection is speaking: a typed
// error event for task-aware clients, a legacy Status for old ones.
func replyError(c *client, id, msg string) {
	if c.isTaskMode() {
		c.send(Event{Type: "error", ID: id, Err: msg})
		return
	}
	c.send(Status{Err: msg})
}

func handleFiles(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	if prefix == "" {
		prefix = "."
	}
	prefix = strings.TrimSuffix(prefix, "/")

	wd, _ := os.Getwd()
	resp := &FilesResponse{
		Cwd:     wd,
		Folders: []string{},
		Files:   []file{},
	}

	files, _ := os.ReadDir(cleanPath(prefix))
	for _, f := range files {
		if f.IsDir() {
			if prefix == "." {
				resp.Folders = append(resp.Folders, f.Name()+"/")
			} else {
				resp.Folders = append(resp.Folders, prefix+"/"+f.Name()+"/")
			}
		} else {
			info, err := f.Info()
			if err != nil {
				continue // Removed since the directory was read.
			}
			obj := file{Name: prefix + "/" + f.Name(), Size: info.Size()}
			if prefix == "." {
				obj.Name = f.Name()
			}
			resp.Files = append(resp.Files, obj)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	cors(w, r)
	json.NewEncoder(w).Encode(resp)
}

func cleanPath(path string) string {
	// Clean path to make safe for use with filepath.Join.
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		path = filepath.Clean(string(os.PathSeparator) + path)
		path, _ = filepath.Rel(string(os.PathSeparator), path)
	}
	return filepath.Clean(path)
}

func cors(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Origin")
	if origin := r.Header.Get("Origin"); isAllowedOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
}

func verifyFFmpeg() error {
	f := &ffmpeg.FFmpeg{}
	version, err := f.Version()
	if err != nil {
		return err
	}
	fmt.Println("  Checking FFmpeg version....\u001b[32m" + version + "\u001b[0m")

	probe := &ffmpeg.FFProbe{}
	version, err = probe.Version()
	if err != nil {
		return err
	}
	fmt.Println("  Checking FFprobe version...\u001b[32m" + version + "\u001b[0m\n")
	return nil
}

// totalFrames returns the frame count of the first video stream, or 0.
func totalFrames(p *ffmpeg.FFProbeResponse) int {
	for _, s := range p.Streams {
		if s.CodecType == "video" {
			n, _ := strconv.Atoi(s.NbFrames)
			return n
		}
	}
	return 0
}

// clearLine erases what's left of a progress line after \r.
const clearLine = "\u001b[K"
