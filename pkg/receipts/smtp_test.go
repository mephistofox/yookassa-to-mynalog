package receipts

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSMTPUsesVerifiedTLSAndTreatsDATAAckAsSuccess(t *testing.T) {
	certServer := httptest.NewTLSServer(nil)
	certificate := certServer.TLS.Certificates[0]
	cert := certServer.Certificate()
	certServer.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- "accept failed"
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprint(conn, "220 test ESMTP\r\n")
		scanner := bufio.NewScanner(conn)
		var body strings.Builder
		data := false
		for scanner.Scan() {
			line := scanner.Text()
			if data {
				if line == "." {
					data = false
					fmt.Fprint(conn, "250 queued\r\n")
				} else {
					body.WriteString(line + "\n")
				}
				continue
			}
			switch {
			case strings.HasPrefix(line, "EHLO"):
				fmt.Fprint(conn, "250 test\r\n")
			case strings.HasPrefix(line, "MAIL FROM:"):
				fmt.Fprint(conn, "250 ok\r\n")
			case strings.HasPrefix(line, "RCPT TO:"):
				fmt.Fprint(conn, "250 ok\r\n")
			case line == "DATA":
				data = true
				fmt.Fprint(conn, "354 send data\r\n")
			case line == "QUIT":
				done <- body.String()
				return
			default:
				fmt.Fprint(conn, "500 unsupported\r\n")
			}
		}
		done <- body.String()
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	sender := SMTPSender{Config: Config{SMTPHost: host, SMTPPort: port, SMTPFrom: "receipts@example.com", SMTPTLS: "tls", INN: "123456789012", RequestTimeout: 3 * time.Second}, TLSRoots: roots}
	err = sender.Send(context.Background(), Delivery{ID: "one", Email: "user@example.com", Receipt: "r1", Amount: 1010})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-done:
		if !strings.Contains(body, "Message-ID:") || !strings.Contains(body, "Content-Transfer-Encoding: base64") {
			t.Fatal("SMTP payload missing receipt message")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SMTP peer did not finish")
	}
}
func TestSMTPRejectsUntrustedCertificate(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	sender := SMTPSender{Config: Config{SMTPHost: host, SMTPPort: port, SMTPFrom: "receipts@example.com", SMTPTLS: "tls", RequestTimeout: time.Second}}
	if err := sender.Send(context.Background(), Delivery{ID: "one", Email: "user@example.com", Receipt: "r1"}); err == nil {
		t.Fatal("untrusted TLS certificate accepted")
	}
}
