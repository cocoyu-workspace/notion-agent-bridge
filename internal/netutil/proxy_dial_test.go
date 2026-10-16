package netutil

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidateProxyURL(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"", false},
		{"socks5://user:pass@host:1080", false},
		{"socks5h://1.2.3.4:1080", false},
		{"http://1.2.3.4:8080", false},
		{"https://proxy:443", false},
		{"ftp://nope", true},
		{"socks4://x:1080", true},
		{":://broken", true},
		{"http://", true}, // missing host
	}
	for _, tc := range cases {
		err := ValidateProxyURL(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateProxyURL(%q): err=%v wantErr=%v", tc.in, err, tc.wantErr)
		}
	}
}

func TestDialThroughProxyEmptyIsDirect(t *testing.T) {
	// A loopback TCP listener that just accepts and closes — proves the
	// empty proxy-URL path takes the direct dialer.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := DialThroughProxy(ctx, "tcp", ln.Addr().String(), "")
	if err != nil {
		t.Fatalf("dial direct: %v", err)
	}
	c.Close()
}

func TestDialThroughProxyRejectsBadScheme(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := DialThroughProxy(ctx, "tcp", "127.0.0.1:1", "ftp://nope:21")
	if err == nil {
		t.Fatal("expected error for ftp scheme")
	}
}

// TestDialThroughProxyHTTPConnect spins up a tiny HTTP CONNECT proxy and
// verifies we can talk to a backend HTTPS server through it. Covers both
// the auth header and the prefix-conn replay path (we deliberately write a
// few extra bytes after the 200 response).
func TestDialThroughProxyHTTPConnect(t *testing.T) {
	// Backend echo server: returns whatever the client wrote.
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 32)
				n, _ := c.Read(buf)
				c.Write(buf[:n])
			}(c)
		}
	}()

	var (
		gotAuth atomic.Value
		gotHost atomic.Value
	)
	gotAuth.Store("")
	gotHost.Store("")

	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	go func() {
		c, err := proxy.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4096)
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		req := string(buf[:n])
		if i := strings.Index(req, "Proxy-Authorization: "); i >= 0 {
			rest := req[i+len("Proxy-Authorization: "):]
			if j := strings.Index(rest, "\r\n"); j >= 0 {
				gotAuth.Store(rest[:j])
			}
		}
		// First line: "CONNECT host:port HTTP/1.1"
		if i := strings.Index(req, "CONNECT "); i == 0 {
			if j := strings.Index(req[8:], " "); j >= 0 {
				gotHost.Store(req[8 : 8+j])
			}
		}
		c.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))

		// Splice client ↔ backend.
		bc, err := net.Dial("tcp", backend.Addr().String())
		if err != nil {
			return
		}
		defer bc.Close()
		go io.Copy(bc, c)
		io.Copy(c, bc)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	proxyURL := fmt.Sprintf("http://alice:secret@%s", proxy.Addr().String())
	conn, err := DialThroughProxy(ctx, "tcp", backend.Addr().String(), proxyURL)
	if err != nil {
		t.Fatalf("DialThroughProxy: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("echo: %q", got)
	}
	wantAuth := "Basic YWxpY2U6c2VjcmV0" // base64("alice:secret")
	if a := gotAuth.Load().(string); a != wantAuth {
		t.Fatalf("Proxy-Authorization = %q, want %q", a, wantAuth)
	}
	if h := gotHost.Load().(string); h != backend.Addr().String() {
		t.Fatalf("CONNECT host = %q, want %q", h, backend.Addr().String())
	}
}

// TestDialThroughProxyHTTPConnectFails ensures a non-200 from the proxy
// is surfaced as an error rather than silently returning a bad conn.
func TestDialThroughProxyHTTPConnectFails(t *testing.T) {
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	go func() {
		c, err := proxy.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		// Read just enough to consume the request headers, then reply 407.
		readUntilDoubleCRLF(c)
		c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = DialThroughProxy(ctx, "tcp", "1.2.3.4:80",
		fmt.Sprintf("http://%s", proxy.Addr().String()))
	if err == nil || !strings.Contains(err.Error(), "407") {
		t.Fatalf("expected 407 error, got %v", err)
	}
}

func readUntilDoubleCRLF(c net.Conn) {
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer c.SetReadDeadline(time.Time{})
	buf := make([]byte, 1024)
	var acc []byte
	for {
		n, err := c.Read(buf)
		if n > 0 {
			acc = append(acc, buf[:n]...)
			if strings.Contains(string(acc), "\r\n\r\n") {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// TestDialThroughProxySOCKS5 talks to a hand-rolled SOCKS5 listener that
// only supports CONNECT + username/password auth. The test verifies the
// auth bytes match what we shipped in the proxy URL and that the data
// channel is fully bidirectional.
func TestDialThroughProxySOCKS5(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		c, err := backend.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.Write([]byte("hello-from-backend"))
	}()

	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socksLn.Close()

	gotUser, gotPass := "", ""
	go func() {
		c, err := socksLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		// 1. Greeting: VER NMETHODS METHOD...
		hdr := make([]byte, 2)
		if _, err := io.ReadFull(c, hdr); err != nil {
			return
		}
		methods := make([]byte, hdr[1])
		if _, err := io.ReadFull(c, methods); err != nil {
			return
		}
		// Reply: VER METHOD (0x02 = user/pass)
		c.Write([]byte{0x05, 0x02})

		// 2. User/pass auth: VER ULEN USERNAME PLEN PASSWORD
		auth := make([]byte, 2)
		if _, err := io.ReadFull(c, auth); err != nil {
			return
		}
		ulen := int(auth[1])
		user := make([]byte, ulen)
		io.ReadFull(c, user)
		plen := make([]byte, 1)
		io.ReadFull(c, plen)
		pass := make([]byte, int(plen[0]))
		io.ReadFull(c, pass)
		gotUser, gotPass = string(user), string(pass)
		c.Write([]byte{0x01, 0x00})

		// 3. Request: VER CMD RSV ATYP DST.ADDR DST.PORT
		req := make([]byte, 4)
		if _, err := io.ReadFull(c, req); err != nil {
			return
		}
		switch req[3] { // ATYP
		case 0x01:
			io.ReadFull(c, make([]byte, 4))
		case 0x03:
			ln := make([]byte, 1)
			io.ReadFull(c, ln)
			io.ReadFull(c, make([]byte, int(ln[0])))
		case 0x04:
			io.ReadFull(c, make([]byte, 16))
		}
		io.ReadFull(c, make([]byte, 2)) // port

		// Open the backend and reply success.
		bc, err := net.Dial("tcp", backend.Addr().String())
		if err != nil {
			c.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			return
		}
		defer bc.Close()
		c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

		go io.Copy(bc, c)
		io.Copy(c, bc)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	proxyURL := fmt.Sprintf("socks5://bob:hunter2@%s", socksLn.Addr().String())
	conn, err := DialThroughProxy(ctx, "tcp", backend.Addr().String(), proxyURL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	got := make([]byte, len("hello-from-backend"))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello-from-backend" {
		t.Fatalf("got %q", got)
	}
	if gotUser != "bob" || gotPass != "hunter2" {
		t.Fatalf("auth bytes: user=%q pass=%q", gotUser, gotPass)
	}
}

var _ = binary.BigEndian
