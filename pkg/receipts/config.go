// Copyright 2026 fxTunnel contributors. Licensed under Apache-2.0.
package receipts

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DBPath, ShopID, APIKey                                        string
	AuthMethod, Login, Password, RefreshToken, DeviceID, INN      string
	SMTPHost, SMTPPort, SMTPUser, SMTPPassword, SMTPFrom, SMTPTLS string
	DeliveryMode, Description, Listen                             string
	YooSourceIP, NalogSourceIP                                    string
	Start                                                         time.Time
	PollInterval, RetryInterval, RequestTimeout, HealthMaxAge     time.Duration
	RefundsEnabled                                                bool
	Manual                                                        bool
	FNSInterval                                                   time.Duration
}

func LoadConfig() (Config, error) {
	var errs []error
	get := func(k, fallback string) string {
		v := os.Getenv(k)
		if f := os.Getenv(k + "_FILE"); f != "" {
			if v != "" {
				errs = append(errs, fmt.Errorf("set only %s or %s_FILE", k, k))
				return ""
			}
			b, e := os.ReadFile(f)
			if e != nil {
				errs = append(errs, fmt.Errorf("cannot read %s_FILE", k))
				return ""
			}
			v = strings.TrimSpace(string(b))
		}
		if v == "" {
			return fallback
		}
		return v
	}
	duration := func(k, d string) time.Duration {
		v, e := time.ParseDuration(get(k, d))
		if e != nil || v < time.Second {
			errs = append(errs, fmt.Errorf("invalid %s", k))
		}
		return v
	}
	c := Config{
		DBPath: get("STATE_DB", "data/receipts-go.db"), ShopID: get("YOOKASSA_SHOP_ID", ""), APIKey: get("YOOKASSA_API_KEY", ""),
		AuthMethod: get("MOY_NALOG_AUTH_METHOD", "refresh"), Login: get("MOY_NALOG_LOGIN", ""), Password: get("MOY_NALOG_PASSWORD", ""),
		RefreshToken: get("MOY_NALOG_REFRESH_TOKEN", ""), DeviceID: get("DEVICE_ID", ""), INN: get("MOY_NALOG_RECEIPT_INN", ""),
		SMTPHost: get("SMTP_HOST", ""), SMTPPort: get("SMTP_PORT", "587"), SMTPUser: get("SMTP_USER", ""), SMTPPassword: get("SMTP_PASSWORD", ""),
		SMTPFrom: get("SMTP_FROM", ""), SMTPTLS: get("SMTP_TLS", "starttls"), DeliveryMode: get("DELIVERY_MODE", "smtp"),
		Description: get("INCOME_DESCRIPTION", "Доступ к сервису fxTunnel"), Listen: get("HEALTH_LISTEN", "127.0.0.1:8099"),
		YooSourceIP: get("YOOKASSA_SOURCE_IP", ""), NalogSourceIP: get("NALOG_SOURCE_IP", ""),
		PollInterval: duration("POLL_INTERVAL", "1m"), RetryInterval: duration("RETRY_INTERVAL", "15s"), RequestTimeout: duration("REQUEST_TIMEOUT", "30s"), HealthMaxAge: duration("HEALTH_MAX_AGE", "15m"),
		FNSInterval: duration("FNS_REQUEST_INTERVAL", "1s"),
	}
	var e error
	c.RefundsEnabled, e = strconv.ParseBool(get("REFUNDS_ENABLED", "false"))
	if e != nil {
		errs = append(errs, errors.New("invalid REFUNDS_ENABLED"))
	}
	c.Manual, e = strconv.ParseBool(get("MANUAL_MODE", "false"))
	if e != nil {
		errs = append(errs, errors.New("invalid MANUAL_MODE"))
	}
	s := get("SYNC_START_DATE", "")
	c.Start, e = time.Parse(time.RFC3339, s)
	if e != nil {
		errs = append(errs, errors.New("SYNC_START_DATE must be an explicit RFC3339 cutoff, e.g. 2026-10-17T10:00:00+03:00"))
	}
	for k, v := range map[string]string{"YOOKASSA_SHOP_ID": c.ShopID, "YOOKASSA_API_KEY": c.APIKey, "DEVICE_ID": c.DeviceID, "MOY_NALOG_RECEIPT_INN": c.INN} {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", k))
		}
	}
	if len(c.INN) != 12 || strings.Trim(c.INN, "0123456789") != "" {
		errs = append(errs, errors.New("MOY_NALOG_RECEIPT_INN must contain 12 digits"))
	}
	switch c.AuthMethod {
	case "refresh":
		if c.RefreshToken == "" {
			errs = append(errs, errors.New("MOY_NALOG_REFRESH_TOKEN is required"))
		}
	case "password":
		if c.Login == "" || c.Password == "" {
			errs = append(errs, errors.New("MOY_NALOG_LOGIN and MOY_NALOG_PASSWORD are required"))
		}
	default:
		errs = append(errs, errors.New("invalid MOY_NALOG_AUTH_METHOD"))
	}
	if c.DeliveryMode == "smtp" {
		if c.SMTPHost == "" || !validEmail(c.SMTPFrom) {
			errs = append(errs, errors.New("SMTP_HOST and valid SMTP_FROM are required"))
		}
		if c.SMTPTLS != "starttls" && c.SMTPTLS != "tls" {
			errs = append(errs, errors.New("SMTP_TLS must be starttls or tls"))
		}
		port, e := strconv.Atoi(c.SMTPPort)
		if e != nil || port < 1 || port > 65535 {
			errs = append(errs, errors.New("invalid SMTP_PORT"))
		}
		if (c.SMTPUser == "") != (c.SMTPPassword == "") {
			errs = append(errs, errors.New("SMTP_USER and SMTP_PASSWORD must be set together"))
		}
	} else if c.DeliveryMode != "disabled" {
		errs = append(errs, errors.New("DELIVERY_MODE must be smtp or disabled"))
	}
	for k, v := range map[string]string{"YOOKASSA_SOURCE_IP": c.YooSourceIP, "NALOG_SOURCE_IP": c.NalogSourceIP} {
		if v != "" && net.ParseIP(v) == nil {
			errs = append(errs, fmt.Errorf("invalid %s", k))
		}
	}
	if strings.TrimSpace(c.Description) == "" || len([]rune(c.Description)) > 160 {
		errs = append(errs, errors.New("INCOME_DESCRIPTION must contain 1..160 characters"))
	}
	if _, _, e := net.SplitHostPort(c.Listen); e != nil {
		errs = append(errs, errors.New("invalid HEALTH_LISTEN"))
	}
	return c, errors.Join(errs...)
}
