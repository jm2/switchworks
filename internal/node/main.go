// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jm2/switchworks/internal/harness"
	"github.com/jm2/switchworks/internal/paths"
	"github.com/jm2/switchworks/internal/pki"
	"github.com/jm2/switchworks/internal/runstatus"
	"github.com/jm2/switchworks/internal/version"
)

const configFile = "node.json"

// LoadConfig reads node.json.
func LoadConfig(dir string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(dir, configFile))
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

// Main implements `swk node ...`.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: swk node enroll - | swk node run [flags]")
		return 2
	}
	switch args[0] {
	case "enroll":
		return mainEnroll(args[1:], stdout, stderr)
	case "run":
		return mainRun(args[1:], stderr)
	}
	fmt.Fprintf(stderr, "unknown node command %q\n", args[0])
	return 2
}

// enrollResponse mirrors coord.EnrollResponse (kept separate to avoid an
// import cycle between node and coordinator).
type enrollResponse struct {
	Host     string `json:"host"`
	Cert     string `json:"cert"`
	CA       string `json:"ca"`
	NodeAddr string `json:"node_addr"`
}

// Enroll exchanges a one-time token for this host's certificate and writes
// node.json, node.key, node.crt and ca.crt into dir.
func Enroll(ctx context.Context, dir, token string, force bool) (Config, error) {
	var cfg Config
	tok, err := pki.DecodeToken(token)
	if err != nil {
		return cfg, err
	}
	if _, err := os.Stat(filepath.Join(dir, configFile)); err == nil && !force {
		return cfg, fmt.Errorf("%s already enrolled (%s exists); use --force to re-enroll", dir, configFile)
	}
	keyPEM, csrPEM, err := pki.NewNodeKey(tok.Host)
	if err != nil {
		return cfg, err
	}
	apiHost, _, err := net.SplitHostPort(tok.API)
	if err != nil {
		return cfg, fmt.Errorf("token API address: %w", err)
	}
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		TLSClientConfig: pki.PinnedTLS(tok.Fingerprint, apiHost), Proxy: nil}}
	body, _ := json.Marshal(map[string]string{"secret": tok.Secret, "csr": string(csrPEM)})
	req, err := http.NewRequestWithContext(ctx, "POST", "https://"+tok.API+"/v1/enroll", bytes.NewReader(body))
	if err != nil {
		return cfg, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return cfg, fmt.Errorf("enroll with %s: %w", tok.API, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return cfg, fmt.Errorf("enroll with %s: HTTP %d: %s", tok.API, resp.StatusCode, bytes.TrimSpace(b))
	}
	var er enrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return cfg, err
	}
	if er.Host != tok.Host {
		return cfg, errors.New("coordinator issued a certificate for a different host")
	}
	if blk, _ := pem.Decode([]byte(er.CA)); blk == nil {
		return cfg, errors.New("coordinator sent no CA certificate")
	} else if c, err := x509.ParseCertificate(blk.Bytes); err != nil || pki.Fingerprint(c) != tok.Fingerprint {
		return cfg, errors.New("coordinator sent a CA that does not match the token's fingerprint")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return cfg, err
	}
	for name, data := range map[string][]byte{"node.key": keyPEM, "node.crt": []byte(er.Cert), "ca.crt": []byte(er.CA)} {
		if err := paths.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return cfg, err
		}
	}
	// Re-enrolling keeps the rest of an existing node.json (harnesses,
	// defaults) and only points it at the new identity.
	cfg = Config{DefaultApprovalPolicy: "on-request", DefaultSandbox: "workspace-write"}
	if b, err := os.ReadFile(filepath.Join(dir, configFile)); err == nil {
		var old Config
		if json.Unmarshal(b, &old) == nil {
			cfg = old
		}
	}
	cfg.Host, cfg.CoordinatorAPI, cfg.CoordinatorNode = er.Host, tok.API, er.NodeAddr
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := paths.WriteFile(filepath.Join(dir, configFile), append(b, '\n'), 0o600); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func mainEnroll(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("node enroll", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", paths.NodeDir(), "node directory")
	force := fs.Bool("force", false, "re-enroll even if this node is already enrolled")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// The token can come from stdin ("-") or $SWK_ENROLL_TOKEN instead of
	// the command line, where other users of the machine could read it.
	token := os.Getenv("SWK_ENROLL_TOKEN")
	switch {
	case fs.NArg() == 1 && fs.Arg(0) == "-":
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		token = strings.TrimSpace(string(b))
	case fs.NArg() == 1:
		token = fs.Arg(0)
		fmt.Fprintln(stderr, "swk: warning: a token on the command line is visible to other users of this machine (ps); pass - and paste it on stdin, or set SWK_ENROLL_TOKEN")
	case fs.NArg() == 0 && token != "":
	default:
		fmt.Fprintln(stderr, "usage: swk node enroll [--dir DIR] [--force] -   (paste the token on stdin)\n       SWK_ENROLL_TOKEN=... swk node enroll [--dir DIR] [--force]")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := Enroll(ctx, *dir, token, *force)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "Enrolled %s with %s (event stream %s); config in %s\nNext: swk service install node (or swk node run in a terminal)\n",
		cfg.Host, cfg.CoordinatorAPI, cfg.CoordinatorNode, *dir)
	return 0
}

// HarnessOptions builds adapter options from node config, writing harness
// logs under dir/logs.
func HarnessOptions(dir string, cfg Config, logf func(string, ...any)) (harness.Options, error) {
	codexConf, err := codexConfig(cfg)
	if err != nil {
		return harness.Options{}, err
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	hostCodexConfig(cfg, codexConf, logf)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		return harness.Options{}, err
	}
	logFile, err := runstatus.OpenPlainLog(filepath.Join(dir, "logs", "harness.log"))
	if err != nil {
		return harness.Options{}, err
	}
	o := harness.Options{CodexBin: cfg.Codex.Bin, CodexArgs: cfg.Codex.Args, CodexConfig: codexConf, CodexPermissions: cfg.Codex.Permissions,
		AgyBin: cfg.Agy.Bin, AgyArgs: cfg.Agy.Args, Agy: cfg.Agy.Options, ACP: cfg.ACP, Stderr: logFile, Logf: logf}
	if cfg.Codex.Home != "" {
		o.CodexEnv = map[string]string{"CODEX_HOME": cfg.Codex.Home}
	}
	return o, nil
}

func mainRun(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("node run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", paths.NodeDir(), "node directory")
	logFile := fs.String("log-file", "", "append the node's log to this file instead of stderr")
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
	logger := log.New(stderr, "node: ", log.LstdFlags)
	cfg, err := LoadConfig(*dir)
	if err != nil {
		logger.Printf("load config (run `swk node enroll -` first): %v", err)
		return 1
	}
	lock, err := runstatus.Lock(*dir)
	if err != nil {
		logger.Print(err)
		return 1
	}
	defer lock.Close()
	hopts, err := HarnessOptions(*dir, cfg, logger.Printf)
	if err != nil {
		logger.Print(err)
		return 1
	}
	n, err := New(*dir, cfg, hopts, logger.Printf)
	if err != nil {
		logger.Print(err)
		return 1
	}
	logger.Printf("%s %s node %s: harnesses %v, coordinator %s", version.Name, version.Version, cfg.Host, n.Adapters(), cfg.CoordinatorNode)
	if slices.Contains(n.Adapters(), "codex") {
		checkCodexHost(logger.Printf)
	}
	ctx, cancel := runstatus.StopContext(context.Background(), *dir)
	defer cancel()
	_ = n.Run(ctx)
	if err := n.Close(); err != nil {
		logger.Printf("close: %v", err)
	}
	logger.Print("stopped")
	return 0
}
