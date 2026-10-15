// SPDX-License-Identifier: GPL-3.0-or-later

package coord

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jm2/switchyard/internal/apiclient"
	"github.com/jm2/switchyard/internal/paths"
	"github.com/jm2/switchyard/internal/pki"
	"github.com/jm2/switchyard/internal/store"
	"github.com/jm2/switchyard/internal/version"
)

// Config is coordinator.json.
type Config struct {
	APIListen      string `json:"api_listen"`  // e.g. ":7480"
	NodeListen     string `json:"node_listen"` // e.g. ":7481"
	PublicAPI      string `json:"public_api"`  // host:port nodes and the CLI dial
	PublicNode     string `json:"public_node"` // host:port nodes dial for the event stream
	AdminTokenHash string `json:"admin_token_hash"`
	Guards         Guards `json:"guards,omitempty"`
	// HTTPListen serves the API and web UI over plain HTTP too, for a
	// reverse proxy on the same machine (e.g. `tailscale serve`). Bind it to
	// loopback.
	HTTPListen string `json:"http_listen,omitempty"`
	// PublicUI is the web UI's URL as browsers reach it, used in login
	// links (default https://<public_api>).
	PublicUI string `json:"public_ui,omitempty"`
}

const configFile = "coordinator.json"

// LoadConfig reads coordinator.json from dir.
func LoadConfig(dir string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(dir, configFile))
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

func writeJSONFile(path string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return paths.WriteFile(path, append(b, '\n'), mode)
}

// Init creates a coordinator directory: CA, server certificate, config and
// an admin token (written into a cli.json in dir). It returns the token.
func Init(dir string, hosts []string, apiPort, nodePort int) (string, error) {
	if len(hosts) == 0 || hosts[0] == "" {
		return "", errors.New("at least one --host (the name nodes use to reach this coordinator) is required")
	}
	if err := pki.InitCA(dir, hosts); err != nil {
		return "", err
	}
	token := pki.RandomSecret(32)
	cfg := Config{
		APIListen:      fmt.Sprintf(":%d", apiPort),
		NodeListen:     fmt.Sprintf(":%d", nodePort),
		PublicAPI:      net.JoinHostPort(hosts[0], fmt.Sprint(apiPort)),
		PublicNode:     net.JoinHostPort(hosts[0], fmt.Sprint(nodePort)),
		AdminTokenHash: pki.HashSecret(token),
	}
	if err := writeJSONFile(filepath.Join(dir, configFile), cfg, 0o600); err != nil {
		return "", err
	}
	ca, err := pki.LoadCA(dir)
	if err != nil {
		return "", err
	}
	cli := apiclient.Config{API: "https://" + cfg.PublicAPI, CA: string(ca.PEM), Token: token}
	if err := writeJSONFile(filepath.Join(dir, "cli.json"), cli, 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// Main implements `swyd coordinator ...`.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: swyd coordinator init|run|enroll-token|cli-config [flags]")
		return 2
	}
	switch args[0] {
	case "init":
		return mainInit(args[1:], stdout, stderr)
	case "run":
		return mainRun(args[1:], stderr)
	case "enroll-token":
		return mainEnrollToken(args[1:], stdout, stderr)
	case "cli-config":
		fs := flag.NewFlagSet("cli-config", flag.ContinueOnError)
		dir := fs.String("dir", paths.CoordinatorDir(), "coordinator directory")
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		b, err := os.ReadFile(filepath.Join(*dir, "cli.json"))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		stdout.Write(b)
		return 0
	}
	fmt.Fprintf(stderr, "unknown coordinator command %q\n", args[0])
	return 2
}

func mainInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("coordinator init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", paths.CoordinatorDir(), "coordinator directory")
	hosts := fs.String("host", "", "comma-separated names/IPs for the server certificate; the first is what nodes and the CLI dial (e.g. coord.lan,10.0.0.5,coord.tailnet.ts.net)")
	apiPort := fs.Int("api-port", 7480, "HTTPS API port")
	nodePort := fs.Int("node-port", 7481, "node stream port")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var hs []string
	for _, h := range strings.Split(*hosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hs = append(hs, h)
		}
	}
	if _, err := Init(*dir, hs, *apiPort, *nodePort); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	userCLI := paths.CLIConfig()
	copied := false
	if _, err := os.Stat(userCLI); os.IsNotExist(err) {
		if b, err := os.ReadFile(filepath.Join(*dir, "cli.json")); err == nil {
			if paths.WriteFile(userCLI, b, 0o600) == nil {
				copied = true
			}
		}
	}
	fmt.Fprintf(stdout, "Initialized %s coordinator in %s\n", version.Name, *dir)
	fmt.Fprintf(stdout, "  API:   https://%s (listen %d)\n  nodes: %s (listen %d)\n", net.JoinHostPort(hs[0], fmt.Sprint(*apiPort)), *apiPort, net.JoinHostPort(hs[0], fmt.Sprint(*nodePort)), *nodePort)
	fmt.Fprintf(stdout, "  CLI config with the admin token: %s\n", filepath.Join(*dir, "cli.json"))
	if copied {
		fmt.Fprintf(stdout, "  also installed for this user at %s\n", userCLI)
	}
	fmt.Fprintf(stdout, "\nNext: swyd coordinator run, then on each workstation:\n  swyd node enroll $(swyd coordinator enroll-token <host>)\n")
	return 0
}

func mainRun(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("coordinator run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", paths.CoordinatorDir(), "coordinator directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger := log.New(stderr, "coordinator: ", log.LstdFlags)
	cfg, err := LoadConfig(*dir)
	if err != nil {
		logger.Printf("load config (run `swyd coordinator init` first): %v", err)
		return 1
	}
	st, err := store.OpenFileStore(filepath.Join(*dir, "data"))
	if err != nil {
		logger.Printf("open store: %v", err)
		return 1
	}
	defer st.Close()
	srv, err := NewServer(*dir, cfg, st, logger.Printf)
	if err != nil {
		logger.Print(err)
		return 1
	}
	apiLn, err := net.Listen("tcp", cfg.APIListen)
	if err != nil {
		logger.Print(err)
		return 1
	}
	nodeLn, err := net.Listen("tcp", cfg.NodeListen)
	if err != nil {
		logger.Print(err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		if err := srv.ServeNodes(nodeLn); err != nil && ctx.Err() == nil {
			logger.Printf("node listener: %v", err)
			cancel()
		}
	}()
	if cfg.HTTPListen != "" {
		// Plain HTTP carries the admin token and cookies unencrypted, and
		// requests from loopback are trusted to say what a proxy forwarded.
		if h, _, err := net.SplitHostPort(cfg.HTTPListen); err != nil || (h != "localhost" && (net.ParseIP(h) == nil || !net.ParseIP(h).IsLoopback())) {
			logger.Printf("http_listen %q must be a loopback address (127.0.0.1:port or [::1]:port)", cfg.HTTPListen)
			return 1
		}
		plainLn, err := net.Listen("tcp", cfg.HTTPListen)
		if err != nil {
			logger.Print(err)
			return 1
		}
		go func() {
			if err := srv.ServePlain(ctx, plainLn); err != nil && ctx.Err() == nil {
				logger.Printf("plain HTTP listener: %v", err)
			}
		}()
		logger.Printf("plain HTTP (for a local reverse proxy) on %s", cfg.HTTPListen)
	}
	ui := cfg.PublicUI
	if ui == "" {
		ui = "https://" + cfg.PublicAPI
	}
	logger.Printf("%s %s: API on %s, nodes on %s, web UI at %s", version.Name, version.Version, cfg.APIListen, cfg.NodeListen, ui)
	if err := srv.ServeAPI(ctx, apiLn); err != nil {
		logger.Printf("API: %v", err)
	}
	srv.Shutdown()
	logger.Print("stopped")
	return 0
}

func mainEnrollToken(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("coordinator enroll-token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", paths.CoordinatorDir(), "coordinator directory (for its cli.json)")
	ttl := fs.Duration("ttl", time.Hour, "token lifetime")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: swyd coordinator enroll-token [--ttl 1h] <host>")
		return 2
	}
	cfgPath := filepath.Join(*dir, "cli.json")
	if _, err := os.Stat(cfgPath); err != nil {
		cfgPath = paths.CLIConfig()
	}
	cc, err := apiclient.LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	c, err := apiclient.New(cc)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tok, err := c.EnrollToken(ctx, fs.Arg(0), *ttl)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, tok)
	return 0
}
