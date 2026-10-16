// Copyright 2026 fxTunnel contributors. Licensed under Apache-2.0.
package receipts

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"net/url"
	"strings"
	"time"
)

type SMTPSender struct {
	Config Config
	// Optional custom trust roots for an embedded deployment. Certificate
	// verification remains mandatory; the standalone service uses system roots.
	TLSRoots *x509.CertPool
}

func ReceiptURL(inn, id string) string {
	return "https://lknpd.nalog.ru/api/v1/receipt/" + url.PathEscape(inn) + "/" + url.PathEscape(id) + "/print"
}
func mailMessage(c Config, d Delivery) []byte {
	subject := "Чек за оплату fxTunnel"
	body := fmt.Sprintf("Спасибо за оплату fxTunnel.\n\nСумма: %s ₽\nЧек самозанятого: %s\n", d.Amount.String(), ReceiptURL(c.INN, d.Receipt))
	if strings.HasPrefix(d.ID, "refund:") {
		subject = "Чек fxTunnel после возврата"
		body = fmt.Sprintf("После частичного возврата оформлен новый чек на остаток: %s ₽.\nПредыдущий чек аннулирован.\n\nНовый чек: %s\n", d.Amount.String(), ReceiptURL(c.INN, d.Receipt))
	}
	if d.Cancelled {
		subject = "Чек fxTunnel аннулирован"
		body = "В связи с полным возвратом средств чек аннулирован.\n\nЧек: " + ReceiptURL(c.INN, d.Receipt) + "\n"
	}
	hash := sha256.Sum256([]byte(d.ID + ":" + d.Receipt))
	domain := strings.SplitN(c.SMTPFrom, "@", 2)[1]
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%x@%s>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n", c.SMTPFrom, d.Email, mime.QEncoding.Encode("utf-8", subject), time.Now().Format(time.RFC1123Z), hash, domain)
	encoded := base64.StdEncoding.EncodeToString([]byte(body))
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return []byte(b.String())
}
func (s *SMTPSender) Send(ctx context.Context, d Delivery) error {
	c := s.Config
	if !validEmail(c.SMTPFrom) || !validEmail(d.Email) {
		return fmt.Errorf("invalid SMTP address")
	}
	dialer := net.Dialer{Timeout: c.RequestTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(c.SMTPHost, c.SMTPPort))
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(c.RequestTimeout)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	conn.SetDeadline(deadline)
	rawConn := conn
	stop := context.AfterFunc(ctx, func() { rawConn.Close() })
	defer stop()
	tlsConfig := &tls.Config{ServerName: c.SMTPHost, MinVersion: tls.VersionTLS12, RootCAs: s.TLSRoots}
	if c.SMTPTLS == "tls" {
		tlsConn := tls.Client(conn, tlsConfig)
		if err = tlsConn.HandshakeContext(ctx); err != nil {
			return err
		}
		conn = tlsConn
	}
	client, err := smtp.NewClient(conn, c.SMTPHost)
	if err != nil {
		return err
	}
	defer client.Close()
	if c.SMTPTLS == "starttls" {
		if err = client.StartTLS(tlsConfig); err != nil {
			return err
		}
	} else if c.SMTPTLS != "tls" {
		return fmt.Errorf("SMTP requires TLS")
	}
	if c.SMTPUser != "" {
		if err = client.Auth(smtp.PlainAuth("", c.SMTPUser, c.SMTPPassword, c.SMTPHost)); err != nil {
			return err
		}
	}
	if err = client.Mail(c.SMTPFrom); err != nil {
		return err
	}
	if err = client.Rcpt(d.Email); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = w.Write(mailMessage(c, d)); err != nil {
		w.Close()
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	// DATA was acknowledged: failure to close the SMTP session must not resend.
	_ = client.Quit()
	return nil
}
