// Package notify delivers opt-in terse SMTP notifications over verified TLS.
package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/mail"
	"path/filepath"
	"strings"
)

var ErrConfig = errors.New("INVALID_SMTP_CONFIG")

type Config struct {
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Username     string `json:"username"`
	PasswordFile string `json:"password_file"`
	From         string `json:"from"`
	To           string `json:"to"`
}

func (c Config) Validate() error {
	if c.Host == "" || len(c.Host) > 253 || strings.ContainsAny(c.Host, "/\\\x00\r\n\t ") || c.Port < 1 || c.Port > 65535 {
		return ErrConfig
	}
	if net.ParseIP(c.Host) == nil {
		for _, label := range strings.Split(c.Host, ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return ErrConfig
			}
			for _, ch := range label {
				if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' {
					continue
				}
				return ErrConfig
			}
		}
	}
	for _, address := range []string{c.From, c.To} {
		if len(address) > 254 || strings.ContainsAny(address, "\x00\r\n") {
			return ErrConfig
		}
		parsed, e := mail.ParseAddress(address)
		if e != nil || parsed.Address != address || parsed.Name != "" {
			return ErrConfig
		}
	}
	if (c.Username == "") != (c.PasswordFile == "") || len(c.Username) > 256 || strings.ContainsAny(c.Username, "\x00\r\n") {
		return ErrConfig
	}
	if c.PasswordFile != "" {
		if !filepath.IsAbs(c.PasswordFile) || filepath.Clean(c.PasswordFile) != c.PasswordFile {
			return ErrConfig
		}
		for _, system := range []string{"/dev", "/proc", "/sys"} {
			if c.PasswordFile == system || strings.HasPrefix(c.PasswordFile, system+"/") {
				return ErrConfig
			}
		}
	}
	return nil
}
func (c *Config) UnmarshalJSON(raw []byte) error {
	if len(raw) > 4096 {
		return ErrConfig
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, e := decoder.Token()
	if e != nil || token != json.Delim('{') {
		return ErrConfig
	}
	allowed := map[string]bool{"host": true, "port": true, "username": true, "password_file": true, "from": true, "to": true}
	for decoder.More() {
		key, e := decoder.Token()
		if e != nil {
			return ErrConfig
		}
		name, ok := key.(string)
		if !ok || !allowed[name] {
			return ErrConfig
		}
		delete(allowed, name)
		var value json.RawMessage
		if e = decoder.Decode(&value); e != nil || string(value) == "null" {
			return ErrConfig
		}
	}
	if token, e = decoder.Token(); e != nil || token != json.Delim('}') {
		return ErrConfig
	}
	if _, e = decoder.Token(); e != io.EOF {
		return ErrConfig
	}
	type plain Config
	var parsed plain
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(&parsed); e != nil {
		return ErrConfig
	}
	next := Config(parsed)
	if e = next.Validate(); e != nil {
		return e
	}
	*c = next
	return nil
}
