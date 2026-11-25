package notify

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

type Message struct {
	ID          int64
	JobID, Code string
}
type Result struct{ Accepted bool }
type DeliveryError struct {
	Code      string
	Temporary bool
}

func (e *DeliveryError) Error() string { return e.Code }
func failure(e error) *DeliveryError {
	var certificate *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalidCertificate x509.CertificateInvalidError
	if errors.As(e, &certificate) || errors.As(e, &unknownAuthority) || errors.As(e, &hostname) || errors.As(e, &invalidCertificate) {
		return &DeliveryError{Code: "SMTP_TLS_REJECTED"}
	}
	var response *textproto.Error
	if errors.As(e, &response) {
		return &DeliveryError{Code: "SMTP_REJECTED", Temporary: response.Code >= 400 && response.Code < 500}
	}
	var network net.Error
	if errors.As(e, &network) || errors.Is(e, io.EOF) || errors.Is(e, io.ErrUnexpectedEOF) {
		return &DeliveryError{Code: "SMTP_NETWORK_FAILED", Temporary: true}
	}
	return &DeliveryError{Code: "SMTP_PROTOCOL_FAILED", Temporary: false}
}
func secret(path string) (string, error) {
	fd, e := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return "", ErrConfig
	}
	pinned := os.NewFile(uintptr(fd), "smtp-secret")
	defer pinned.Close()
	info, e := pinned.Stat()
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > 1024 {
		return "", ErrConfig
	}
	file, e := os.Open(fmt.Sprintf("/proc/self/fd/%d", fd))
	if e != nil {
		return "", ErrConfig
	}
	raw, e := io.ReadAll(io.LimitReader(file, 1025))
	e = errors.Join(e, file.Close())
	if e != nil || len(raw) > 1024 {
		return "", ErrConfig
	}
	password := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if password == "" || strings.ContainsAny(password, "\x00\r\n") {
		return "", ErrConfig
	}
	return password, nil
}
func Send(ctx context.Context, c Config, message Message) (Result, error) {
	return send(ctx, c, message, nil)
}
func send(ctx context.Context, c Config, message Message, tlsSettings *tls.Config) (result Result, err error) {
	if e := c.Validate(); e != nil {
		return result, &DeliveryError{Code: "SMTP_INVALID_CONFIG"}
	}
	if message.ID < 1 {
		return result, &DeliveryError{Code: "SMTP_INVALID_MESSAGE"}
	}
	id, e := uuid.Parse(message.JobID)
	if e != nil {
		return result, &DeliveryError{Code: "SMTP_INVALID_MESSAGE"}
	}
	switch message.Code {
	case "WAITING_MEDIA", "SUCCEEDED", "FAILED", "RECOVERY_REQUIRED", "RESTORE_PUBLISHED":
	default:
		return result, &DeliveryError{Code: "SMTP_INVALID_MESSAGE"}
	}
	password := ""
	if c.Username != "" {
		password, e = secret(c.PasswordFile)
		if e != nil {
			return result, &DeliveryError{Code: "SMTP_INVALID_SECRET"}
		}
	}
	if tlsSettings == nil {
		tlsSettings = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.Host}
	}
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: tlsSettings}
	connection, e := dialer.DialContext(ctx, "tcp", net.JoinHostPort(c.Host, strconv.Itoa(c.Port)))
	if e != nil {
		return result, failure(e)
	}
	deadline := time.Now().Add(15 * time.Second)
	if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	if e = connection.SetDeadline(deadline); e != nil {
		connection.Close()
		return result, failure(e)
	}
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	client, e := smtp.NewClient(connection, c.Host)
	if e != nil {
		connection.Close()
		return result, failure(e)
	}
	closed := false
	defer func() {
		if closed {
			return
		}
		closeErr := client.Close()
		if err == nil && closeErr != nil {
			err = &DeliveryError{Code: "SMTP_SESSION_CLOSE_FAILED"}
		}
	}()
	if c.Username != "" {
		if e = client.Auth(smtp.PlainAuth("", c.Username, password, c.Host)); e != nil {
			return result, failure(e)
		}
	}
	if e = client.Mail(c.From); e != nil {
		return result, failure(e)
	}
	if e = client.Rcpt(c.To); e != nil {
		return result, failure(e)
	}
	body, e := client.Data()
	if e != nil {
		return result, failure(e)
	}
	content := "From: " + c.From + "\r\nTo: " + c.To + "\r\nDate: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\nMessage-ID: <tapepilot-" + id.String() + "-" + strconv.FormatInt(message.ID, 10) + "@local.invalid>\r\nSubject: TapePilot job " + message.Code + "\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nJob " + id.String() + "\r\nState " + message.Code + "\r\nOpen your TapePilot console for details.\r\n"
	if _, e = io.WriteString(body, content); e != nil {
		return result, failure(errors.Join(e, body.Close()))
	}
	if e = body.Close(); e != nil {
		return result, failure(e)
	}
	result.Accepted = true
	// DATA was acknowledged. A later session close failure cannot justify replay.
	if e = client.Quit(); e != nil {
		return result, &DeliveryError{Code: "SMTP_SESSION_CLOSE_FAILED"}
	}
	closed = true
	return result, nil
}
