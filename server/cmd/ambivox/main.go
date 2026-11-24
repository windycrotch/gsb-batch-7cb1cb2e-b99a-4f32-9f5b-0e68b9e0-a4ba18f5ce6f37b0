// Command ambivox serves the web client, the signalling endpoint and the
// WebRTC transport, and relays audio to the media engine for translation.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/whispem/ambivox/server/internal/httpapi"
	"github.com/whispem/ambivox/server/internal/hub"
	"github.com/whispem/ambivox/server/internal/invite"
	"github.com/whispem/ambivox/server/internal/lan"
	"github.com/whispem/ambivox/server/internal/medialink"
	"github.com/whispem/ambivox/server/internal/metrics"
	"github.com/whispem/ambivox/server/web"
)

var version = "0.8.0"

type options struct {
	listen      string
	mediaSocket string
	maxRooms    int
	iceServers  string
	publicIPs   string
	udpPorts    string
	udpPort     uint
	iceLoopback bool
	origins     string
	tlsCert     string
	tlsKey      string
	selfSigned  bool
	hosts       string
	dataDir     string
	openRooms   bool
	inviteTTL   time.Duration
	accessKey   string
	echoGuard   bool
	logLevel    string
	logFormat   string
	showVersion bool
}

func env(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

func parseFlags(args []string) (options, error) {
	var o options
	maxRooms, err := strconv.Atoi(env("AMBIVOX_MAX_ROOMS", "100"))
	if err != nil {
		return o, fmt.Errorf("AMBIVOX_MAX_ROOMS: %w", err)
	}
	udpPort, err := strconv.ParseUint(env("AMBIVOX_UDP_PORT", "0"), 10, 16)
	if err != nil {
		return o, fmt.Errorf("AMBIVOX_UDP_PORT: %w", err)
	}
	inviteTTL, err := time.ParseDuration(env("AMBIVOX_INVITE_TTL", "24h"))
	if err != nil {
		return o, fmt.Errorf("AMBIVOX_INVITE_TTL: %w", err)
	}
	o.accessKey = os.Getenv("AMBIVOX_ACCESS_KEY")

	fs := flag.NewFlagSet("ambivox", flag.ContinueOnError)
	fs.StringVar(&o.listen, "listen", env("AMBIVOX_LISTEN", ":8080"), "HTTP listen address")
	fs.StringVar(&o.mediaSocket, "media-socket", env("AMBIVOX_MEDIA_SOCKET", "/tmp/ambivox-media.sock"), "media engine Unix socket")
	fs.IntVar(&o.maxRooms, "max-rooms", maxRooms, "maximum number of simultaneous rooms")
	fs.StringVar(&o.iceServers, "ice-servers", env("AMBIVOX_ICE_SERVERS", ""), "comma-separated STUN/TURN URLs offered to both ends")
	fs.StringVar(&o.publicIPs, "public-ip", env("AMBIVOX_PUBLIC_IP", ""), "comma-separated public IPs to advertise when behind 1:1 NAT")
	fs.StringVar(&o.udpPorts, "udp-ports", env("AMBIVOX_UDP_PORTS", ""), "UDP port range for media, as MIN-MAX")
	fs.UintVar(&o.udpPort, "udp-port", uint(udpPort), "single UDP port shared by every call")
	fs.BoolVar(&o.iceLoopback, "ice-loopback", env("AMBIVOX_ICE_LOOPBACK", "") == "1", "offer loopback ICE candidates (single-machine setups)")
	fs.StringVar(&o.origins, "allow-origin", env("AMBIVOX_ALLOW_ORIGIN", ""), "comma-separated extra origins allowed to open the WebSocket")
	fs.StringVar(&o.tlsCert, "tls-cert", env("AMBIVOX_TLS_CERT", ""), "TLS certificate file (enables HTTPS)")
	fs.StringVar(&o.tlsKey, "tls-key", env("AMBIVOX_TLS_KEY", ""), "TLS private key file")
	fs.BoolVar(&o.selfSigned, "tls-self-signed", env("AMBIVOX_TLS_SELF_SIGNED", "") == "1", "serve HTTPS with a self-signed certificate for the local network")
	fs.StringVar(&o.hosts, "host", env("AMBIVOX_HOST", ""), "comma-separated addresses or names other devices use to reach this server")
	fs.StringVar(&o.dataDir, "data-dir", env("AMBIVOX_DATA_DIR", ""), "directory keeping the certificate and invitation key across restarts")
	fs.BoolVar(&o.openRooms, "open-rooms", env("AMBIVOX_OPEN_ROOMS", "") == "1", "let anyone join any room by name, without an invitation")
	fs.DurationVar(&o.inviteTTL, "invite-ttl", inviteTTL, "how long an invitation link stays valid")
	fs.BoolVar(&o.echoGuard, "echo-guard", env("AMBIVOX_ECHO_GUARD", "1") != "0", "silence a speaker while their translation plays, so it cannot loop back")
	fs.StringVar(&o.logLevel, "log-level", env("AMBIVOX_LOG_LEVEL", "info"), "debug, info, warn or error")
	fs.StringVar(&o.logFormat, "log-format", env("AMBIVOX_LOG_FORMAT", "text"), "text or json")
	fs.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument: %s", fs.Arg(0))
	}
	if (o.tlsCert == "") != (o.tlsKey == "") {
		return o, errors.New("-tls-cert and -tls-key must be given together")
	}
	if o.selfSigned && o.tlsCert != "" {
		return o, errors.New("-tls-self-signed and -tls-cert are mutually exclusive")
	}
	if o.udpPort > 65535 || (o.udpPort != 0 && o.udpPorts != "") {
		return o, errors.New("-udp-port takes one port and excludes -udp-ports")
	}
	if o.inviteTTL <= 0 {
		return o, errors.New("-invite-ttl must be positive")
	}
	return o, nil
}

func splitList(s string) []string {
	var out []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func parsePortRange(s string) (uint16, uint16, error) {
	if s == "" {
		return 0, 0, nil
	}
	low, high, ok := strings.Cut(s, "-")
	lo, err1 := strconv.ParseUint(strings.TrimSpace(low), 10, 16)
	hi, err2 := strconv.ParseUint(strings.TrimSpace(high), 10, 16)
	if !ok || err1 != nil || err2 != nil || lo == 0 || lo > hi {
		return 0, 0, fmt.Errorf("invalid UDP port range %q, want MIN-MAX", s)
	}
	return uint16(lo), uint16(hi), nil
}

func newLogger(level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
	}
	return nil, fmt.Errorf("invalid log format %q", format)
}

func run(args []string) error {
	o, err := parseFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if o.showVersion {
		fmt.Println("ambivox", version)
		return nil
	}
	log, err := newLogger(o.logLevel, o.logFormat)
	if err != nil {
		return err
	}
	portMin, portMax, err := parsePortRange(o.udpPorts)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var invites *invite.Issuer
	if !o.openRooms {
		key, err := invite.LoadKey(o.dataDir)
		if err != nil {
			return fmt.Errorf("invitation key: %w", err)
		}
		invites = invite.New(key, o.inviteTTL)
	}

	hosts := splitList(o.hosts)
	if len(hosts) == 0 {
		hosts = lan.Addresses()
	}
	var tlsConfig *tls.Config
	switch {
	case o.selfSigned:
		cert, err := lan.Certificate(o.dataDir, hosts)
		if err != nil {
			return fmt.Errorf("self-signed certificate: %w", err)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	case o.tlsCert != "":
		cert, err := tls.LoadX509KeyPair(o.tlsCert, o.tlsKey)
		if err != nil {
			return err
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	listener, err := net.Listen("tcp", o.listen)
	if err != nil {
		return err
	}
	registry := metrics.NewRegistry()
	link := medialink.Dial(ctx, o.mediaSocket, log)
	rooms, err := hub.New(hub.Config{
		Link:            link,
		Logger:          log,
		Metrics:         registry,
		MaxRooms:        o.maxRooms,
		ICEServers:      splitList(o.iceServers),
		PublicIPs:       splitList(o.publicIPs),
		UDPPortMin:      portMin,
		UDPPortMax:      portMax,
		UDPPort:         uint16(o.udpPort),
		IncludeLoopback: o.iceLoopback,
		AllowedOrigins:  splitList(o.origins),
		EchoGuard:       o.echoGuard,
		Invites:         invites,
		AccessKey:       o.accessKey,
	})
	if err != nil {
		listener.Close()
		return err
	}

	server := &http.Server{
		Addr:              o.listen,
		Handler:           httpapi.New(rooms, link, registry, web.Static()),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		TLSConfig:         tlsConfig,
	}
	failed := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", listener.Addr().String(), "tls", tlsConfig != nil, "invitations", invites != nil,
			"access_key", o.accessKey != "", "version", version)
		if tlsConfig != nil {
			failed <- server.ServeTLS(listener, "", "")
		} else {
			failed <- server.Serve(listener)
		}
	}()
	announce(os.Stdout, listener.Addr(), tlsConfig, hosts, o.selfSigned)

	select {
	case err := <-failed:
		rooms.Close()
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	rooms.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	<-link.Done()
	return nil
}

func announce(w io.Writer, addr net.Addr, config *tls.Config, hosts []string, selfSigned bool) {
	port := addr.(*net.TCPAddr).Port
	if config == nil {
		fmt.Fprintf(w, "\nambivox %s is ready at http://localhost:%d\n", version, port)
		fmt.Fprintln(w, "Browsers only allow the microphone over HTTPS on other devices: add -tls-self-signed to call from them.")
		return
	}
	fmt.Fprintf(w, "\nambivox %s is ready. Open this on any device on the same network:\n", version)
	for _, host := range append(hosts, "localhost") {
		fmt.Fprintf(w, "  https://%s\n", net.JoinHostPort(host, strconv.Itoa(port)))
	}
	if selfSigned {
		fmt.Fprintln(w, "The certificate is self-signed: each browser asks once to trust it. Its fingerprint is")
		fmt.Fprintf(w, "  SHA-256 %s\n", lan.Fingerprint(config.Certificates[0]))
	}
	fmt.Fprintln(w)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ambivox:", err)
		os.Exit(1)
	}
}
