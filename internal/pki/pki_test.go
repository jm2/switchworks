package pki

import (
	"crypto/tls"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestCAIssueAndMutualTLS(t *testing.T) {
	dir := t.TempDir()
	if err := InitCA(dir, []string{"coord.lan", "10.0.0.5"}); err != nil {
		t.Fatal(err)
	}
	if err := InitCA(dir, nil); err == nil {
		t.Fatal("InitCA must refuse to overwrite")
	}
	if fi, err := os.Stat(filepath.Join(dir, CAKeyFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("ca key perms: %v %v", fi.Mode(), err)
	}
	ca, err := LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, csrPEM, err := NewNodeKey("ignored-subject")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := ca.SignNodeCSR(csrPEM, "antec")
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := ParseCert(certPEM)
	if cert.Subject.CommonName != "antec" {
		t.Fatalf("CN %q", cert.Subject.CommonName)
	}

	srvCfg, err := ServerTLS(dir, ca)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	peer := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		tc := c.(*tls.Conn)
		if err := tc.Handshake(); err != nil {
			peer <- "handshake: " + err.Error()
			c.Close()
			return
		}
		peer <- tc.ConnectionState().PeerCertificates[0].Subject.CommonName
		_, _ = io.WriteString(c, "ok")
		c.Close()
	}()
	cliCfg, err := ClientTLS(ca.PEM, certPEM, keyPEM, "coord.lan")
	if err != nil {
		t.Fatal(err)
	}
	c, err := tls.Dial("tcp", ln.Addr().String(), cliCfg)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(c)
	c.Close()
	if got := <-peer; got != "antec" {
		t.Fatalf("server saw peer %q", got)
	}

	// Without a client certificate the mTLS listener rejects the client.
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	noCert, _ := ClientTLS(ca.PEM, nil, nil, "coord.lan")
	if c, err := tls.Dial("tcp", ln.Addr().String(), noCert); err == nil {
		_, rerr := c.Read(make([]byte, 1))
		c.Close()
		if rerr == nil {
			t.Fatal("client without certificate was accepted")
		}
	}
}

func TestPinnedEnrollmentTLS(t *testing.T) {
	dir := t.TempDir()
	if err := InitCA(dir, []string{"coord.lan"}); err != nil {
		t.Fatal(err)
	}
	ca, _ := LoadCA(dir)
	srvCfg, _ := ServerTLS(dir, nil)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_ = c.(*tls.Conn).Handshake()
				_, _ = io.WriteString(c, "hi")
				c.Close()
			}(c)
		}
	}()
	good := PinnedTLS(Fingerprint(ca.Cert), "coord.lan")
	c, err := tls.Dial("tcp", ln.Addr().String(), good)
	if err != nil {
		t.Fatalf("pinned dial: %v", err)
	}
	c.Close()
	bad := PinnedTLS("00"+Fingerprint(ca.Cert)[2:], "coord.lan")
	if c, err := tls.Dial("tcp", ln.Addr().String(), bad); err == nil {
		c.Close()
		t.Fatal("wrong fingerprint accepted")
	}
	wrongName := PinnedTLS(Fingerprint(ca.Cert), "elsewhere.lan")
	if c, err := tls.Dial("tcp", ln.Addr().String(), wrongName); err == nil {
		c.Close()
		t.Fatal("wrong server name accepted")
	}
}

func TestToken(t *testing.T) {
	tok := Token{API: "coord.lan:7480", Secret: RandomSecret(16), Fingerprint: "ab", Host: "antec"}
	s := tok.Encode()
	got, err := DecodeToken(s)
	if err != nil || got != tok {
		t.Fatalf("round trip: %v %+v", err, got)
	}
	if _, err := DecodeToken("nope"); err == nil {
		t.Fatal("bad prefix accepted")
	}
	if HashSecret("a") == HashSecret("b") {
		t.Fatal("hash collision")
	}
}
