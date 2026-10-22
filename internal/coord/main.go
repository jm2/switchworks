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
	"path/filepath"
	"strings"
	"time"

	"github.com/jm2/switchworks/internal/apiclient"
	"github.com/jm2/switchworks/internal/paths"
	"github.com/jm2/switchworks/internal/pki"
	"github.com/jm2/switchworks/internal/runstatus"
	"github.com/jm2/switchworks/internal/store"
	"github.com/jm2/switchworks/internal/version"
	"github.com/jm2/switchworks/internal/winsvc"
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
	// SessionRetentionDays is how long a stopped or gone session's record
	// and event log are kept after it ended: 0 means the default (90), a
	// negative number forever.
	SessionRetentionDays int `json:"session_retention_days,omitempty"`
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

// Main implements `swk coordinator ...`.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: swk coordinator init|run|enroll-token|cli-config|health [flags]")
		return 2
	}
	switch args[0] {
	case "init":
		return mainInit(args[1:], stdout, stderr)
	case "run":
		return mainRun(args[1:], stderr)
	case "enroll-token":
		return mainEnrollToken(args[1:], stdout, stderr)
	case "health":
		return mainHealth(args[1:], stdout, stderr)
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
	// The admin cli.json goes to this account's own CLI config only when
	// asked, or for an account that runs no agents: the system
	// coordinator's (`swk --system`) and the container's (SWK_USER_CLI=1 in
	// its image). Anywhere else agents may run as this user and read it.
	userCLIDefault := os.Getenv("SWK_SYSTEM") == "1" || os.Getenv("SWK_USER_CLI") == "1"
	wantUserCLI := fs.Bool("user-cli", userCLIDefault, "also install the admin cli.json as this user's CLI config (only where no agents run as this user)")
	noUserCLI := fs.Bool("no-user-cli", false, "do not install the admin cli.json for this user (overrides --user-cli; not installing is the default)")
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
	if _, err := os.Stat(userCLI); *wantUserCLI && !*noUserCLI && os.IsNotExist(err) {
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
	if ca, err := pki.LoadCA(*dir); err == nil {
		// To check the CA a phone or browser downloads from /ca.crt.
		fmt.Fprintf(stdout, "  CA SHA-256 fingerprint (check it when installing ca.crt):\n    %s\n", colonHex(pki.Fingerprint(ca.Cert)))
	}
	if os.Getenv("SWK_SYSTEM") == "1" {
		// `swk --system coordinator init`: the system service's own files.
		fmt.Fprint(stdout, systemNext)
		return 0
	}
	fmt.Fprintf(stdout, "\nNext: swk coordinator run, then on each workstation:\n  ssh <this host> swk coordinator enroll-token <host> | swk node enroll -\n")
	fmt.Fprint(stdout, `
The admin token controls the whole fleet. Keep a copy somewhere safe (a
password manager) and never put it on a machine where agents run: they can
read any file you can. For the CLI on those machines, make a read token:
  swk token create <machine> --scope read --cli-config > cli.json
and install that file as ~/.config/swk/cli.json there. An operate token
would let agents there approve their own prompts and spawn sessions with
any options: keep operate (and --owner) tokens for your own devices that
run no agents.
`)
	fmt.Fprintf(stdout, `If agents will run on this machine too, they can read %s
(its CA key and admin cli.json) unless the coordinator runs as another user.
`, *dir)
	if copied {
		fmt.Fprintf(stdout, "In that case, also replace %s with a read token's cli.json.\n", userCLI)
	} else {
		fmt.Fprintf(stdout, "For the CLI here, save a read token's cli.json as %s\n(or run init with --user-cli where no agents run as you).\n", userCLI)
	}
	return 0
}

// systemNext is what to do after `swk --system coordinator init` (with sudo
// on Linux; from an elevated prompt, without it, on Windows).
const systemNext = `
This is the system coordinator's account and directory: agents running as
you on this machine cannot read them. Next:

  sudo swk service start --system coordinator

For the CLI as yourself on this machine, a token that can only look: agents
running as you can read it, and with an operate token they could approve
their own prompts and spawn sessions with any options.

  mkdir -p ~/.config/swk
  (umask 077; sudo swk --system token create "$USER" --scope read --cli-config > ~/.config/swk/cli.json)

To run a node here too, as you:

  sudo swk --system coordinator enroll-token "$(hostname)" | swk node enroll -
  swk service install

Other workstations enroll the same way, with
` + "`ssh <this host> sudo swk --system coordinator enroll-token <host> | swk node enroll -`" + `.
The admin token (` + "`sudo swk --system coordinator cli-config`" + ` prints its cli.json)
controls the whole fleet: keep a copy in a password manager.

On Windows, run these from an elevated prompt without sudo, and save the
read token's cli.json as %AppData%\swk\cli.json from a normal one. Use
operate (or --owner) tokens only on your own devices that run no agents.
`

// colonHex formats a hex fingerprint the way certificate viewers show it.
func colonHex(h string) string {
	h = strings.ToUpper(h)
	var b strings.Builder
	for i := 0; i+1 < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}

func mainRun(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("coordinator run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", paths.CoordinatorDir(), "coordinator directory")
	logFile := fs.String("log-file", "", "append the coordinator's log to this file instead of stderr")
	winService := fs.Bool("windows-service", false, "run as the Windows service (swk service install --system registers it so)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *logFile != "" {
		f, err := runstatus.OpenLog(*logFile)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer f.Close()
		stderr = f
	}
	if *winService {
		code, err := winsvc.Run(func(ctx context.Context) int { return runCoordinator(ctx, *dir, stderr) })
		if err != nil {
			fmt.Fprintln(stderr, "coordinator:", err)
			return 1
		}
		return code
	}
	return runCoordinator(context.Background(), *dir, stderr)
}

// runCoordinator runs the coordinator in dir until parent ends or the
// process is asked to stop.
func runCoordinator(parent context.Context, dir string, stderr io.Writer) int {
	logger := log.New(stderr, "coordinator: ", log.LstdFlags)
	cfg, err := LoadConfig(dir)
	if err != nil {
		logger.Printf("load config (run `swk coordinator init` first): %v", err)
		return 1
	}
	lock, err := runstatus.Lock(dir)
	if err != nil {
		logger.Print(err)
		return 1
	}
	defer lock.Close()
	st, err := store.OpenFileStore(filepath.Join(dir, "data"))
	if err != nil {
		logger.Printf("open store: %v", err)
		return 1
	}
	defer st.Close()
	srv, err := NewServer(dir, cfg, st, logger.Printf)
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
	ctx, cancel := runstatus.StopContext(parent, dir)
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
	status := runstatus.Start(dir, runstatus.Status{Role: "coordinator", WebUI: strings.TrimSuffix(ui, "/") + "/"})
	defer status.Stop()
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
		fmt.Fprintln(stderr, "usage: swk coordinator enroll-token [--ttl 1h] <host>")
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

// mainHealth checks that the coordinator in dir answers on its API port,
// over loopback and against its own CA: the container's healthcheck.
func mainHealth(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("coordinator health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", paths.CoordinatorDir(), "coordinator directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := LoadConfig(*dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ca, err := pki.LoadCA(*dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	_, port, err := net.SplitHostPort(cfg.APIListen)
	if err != nil {
		fmt.Fprintln(stderr, "api_listen:", err)
		return 1
	}
	c, err := apiclient.New(apiclient.Config{API: "https://" + cfg.PublicAPI, CA: string(ca.PEM), Dial: net.JoinHostPort("127.0.0.1", port)})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	v, err := c.Health(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "unhealthy:", err)
		return 1
	}
	fmt.Fprintln(stdout, "ok", v)
	return 0
}
