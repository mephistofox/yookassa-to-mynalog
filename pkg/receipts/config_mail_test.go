package receipts

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"YOOKASSA_SHOP_ID", "YOOKASSA_API_KEY", "DEVICE_ID", "MOY_NALOG_REFRESH_TOKEN", "MOY_NALOG_RECEIPT_INN", "SYNC_START_DATE", "DELIVERY_MODE", "MOY_NALOG_AUTH_METHOD"} {
		t.Setenv(key+"_FILE", "")
	}
	t.Setenv("YOOKASSA_SHOP_ID", "shop")
	t.Setenv("YOOKASSA_API_KEY", "key")
	t.Setenv("DEVICE_ID", "device")
	t.Setenv("MOY_NALOG_REFRESH_TOKEN", "token")
	t.Setenv("MOY_NALOG_AUTH_METHOD", "refresh")
	t.Setenv("MOY_NALOG_RECEIPT_INN", "123456789012")
	t.Setenv("SYNC_START_DATE", "2026-10-17T10:00:00+03:00")
	t.Setenv("DELIVERY_MODE", "disabled")
}
func TestConfigRequiresCutoffAndSupportsSecretFiles(t *testing.T) {
	validEnv(t)
	if _, err := LoadConfig(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYNC_START_DATE", "")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("implicit historic issuance allowed")
	}
	t.Setenv("SYNC_START_DATE", "2026-10-17T10:00:00Z")
	secret := filepath.Join(t.TempDir(), "token")
	os.WriteFile(secret, []byte("file-secret\n"), 0600)
	t.Setenv("MOY_NALOG_REFRESH_TOKEN_FILE", secret)
	if _, err := LoadConfig(); err == nil {
		t.Fatal("ambiguous secret sources accepted")
	}
	t.Setenv("MOY_NALOG_REFRESH_TOKEN", "")
	c, err := LoadConfig()
	if err != nil || c.RefreshToken != "file-secret" {
		t.Fatal("secret file not loaded", err)
	}
}
func TestConfigRejectsPlaintextSMTPAndInvalidIP(t *testing.T) {
	validEnv(t)
	t.Setenv("DELIVERY_MODE", "smtp")
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_FROM", "receipts@example.com")
	t.Setenv("SMTP_TLS", "none")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("plaintext SMTP accepted")
	}
	t.Setenv("SMTP_TLS", "starttls")
	t.Setenv("YOOKASSA_SOURCE_IP", "bad-ip")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("invalid source IP accepted")
	}
}
func TestMailHasStableIDAndUTF8ReceiptLink(t *testing.T) {
	c := Config{SMTPFrom: "receipts@example.com", INN: "123456789012"}
	d := Delivery{ID: "one", Email: "user@example.com", Receipt: "r1", Amount: 1010}
	parse := func(data []byte) *mail.Message {
		t.Helper()
		m, err := mail.ReadMessage(bufio.NewReader(bytes.NewReader(data)))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	a, b := parse(mailMessage(c, d)), parse(mailMessage(c, d))
	if a.Header.Get("Message-ID") != b.Header.Get("Message-ID") {
		t.Fatal("unstable delivery identity")
	}
	decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, a.Body))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(decoded), "10.10 ₽") || !strings.Contains(string(decoded), "https://lknpd.nalog.ru/api/v1/receipt/123456789012/r1/print") {
		t.Fatal("mail omitted receipt or exact amount")
	}
	for _, addr := range []string{"user@example.com\r\nBcc: attacker@example.com", "Name <user@example.com>", ""} {
		if validEmail(addr) {
			t.Fatal("invalid envelope accepted")
		}
	}
}
