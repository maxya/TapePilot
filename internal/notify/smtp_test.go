package notify

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/store"
)

func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	pub, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	certificate := x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Synthetic SMTP"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	raw, e := x509.CreateCertificate(rand.Reader, &certificate, &certificate, pub, key)
	if e != nil {
		t.Fatal(e)
	}
	keyBytes, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	pair, e := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}))
	if e != nil {
		t.Fatal(e)
	}
	pool := x509.NewCertPool()
	parsed, e := x509.ParseCertificate(raw)
	if e != nil {
		t.Fatal(e)
	}
	pool.AddCert(parsed)
	return pair, pool
}
func TestLocalTLSOnlyDeliveryAndTerseMessage(t *testing.T) {
	pair, pool := testCertificate(t)
	listener, e := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	received := make(chan string, 1)
	serverErr := make(chan error, 1)
	go func() {
		connection, e := listener.Accept()
		if e != nil {
			serverErr <- e
			return
		}
		defer connection.Close()
		connection.SetDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(connection)
		writer := bufio.NewWriter(connection)
		reply := func(text string) error {
			if _, e := writer.WriteString(text + "\r\n"); e != nil {
				return e
			}
			return writer.Flush()
		}
		if e = reply("220 local SMTP"); e != nil {
			serverErr <- e
			return
		}
		for {
			line, e := reader.ReadString('\n')
			if e != nil {
				serverErr <- e
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO "):
				e = reply("250-local\r\n250 AUTH PLAIN")
			case strings.HasPrefix(line, "AUTH PLAIN "):
				e = reply("235 authenticated")
			case strings.HasPrefix(line, "MAIL FROM:"), strings.HasPrefix(line, "RCPT TO:"):
				e = reply("250 accepted")
			case line == "DATA\r\n":
				if e = reply("354 message"); e != nil {
					serverErr <- e
					return
				}
				var content strings.Builder
				for {
					part, readErr := reader.ReadString('\n')
					if readErr != nil {
						serverErr <- readErr
						return
					}
					if part == ".\r\n" {
						break
					}
					content.WriteString(part)
				}
				received <- content.String()
				e = reply("250 accepted")
			case line == "QUIT\r\n":
				serverErr <- reply("221 closing")
				return
			default:
				serverErr <- fmt.Errorf("unexpected command %q", line)
				return
			}
			if e != nil {
				serverErr <- e
				return
			}
		}
	}()
	_, portText, e := net.SplitHostPort(listener.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	port, e := strconv.Atoi(portText)
	if e != nil {
		t.Fatal(e)
	}
	secretPath := filepath.Join(t.TempDir(), "password")
	if e = os.WriteFile(secretPath, []byte("synthetic-smtp-secret\n"), 0600); e != nil {
		t.Fatal(e)
	}
	config := Config{Host: "127.0.0.1", Port: port, Username: "test-user", PasswordFile: secretPath, From: "tapepilot@example.invalid", To: "operator@example.invalid"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, e := send(ctx, config, Message{ID: 1, JobID: "2f2d805c-e530-4f68-ab29-5cb4c8f63319", Code: "SUCCEEDED"}, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "127.0.0.1", RootCAs: pool})
	if e != nil || !result.Accepted {
		t.Fatal(result, e)
	}
	content := <-received
	if !strings.Contains(content, "State SUCCEEDED") || !strings.Contains(content, "Message-ID:") || strings.Contains(content, "synthetic-smtp-secret") || strings.Contains(content, "test-user") || strings.Contains(content, secretPath) {
		t.Fatal("message includes sensitive content")
	}
	if e = <-serverErr; e != nil {
		t.Fatal(e)
	}
}
func TestSMTPConfigurationAndErrorClassification(t *testing.T) {
	good := `{"host":"relay.example.invalid","port":465,"username":"","password_file":"","from":"tapepilot@example.invalid","to":"operator@example.invalid"}`
	var c Config
	if e := json.Unmarshal([]byte(good), &c); e != nil {
		t.Fatal(e)
	}
	for _, raw := range []string{strings.Replace(good, `"host":`, `"host":null,"host":`, 1), strings.Replace(good, "host", "Host", 1), strings.Replace(good, `"port":465`, `"port":0`, 1), strings.Replace(good, "operator@example.invalid", "operator@example.invalid\\r\\nBcc: other@example.invalid", 1), strings.Replace(good, "password_file", "password", 1)} {
		if e := json.Unmarshal([]byte(raw), &c); e == nil {
			t.Fatal("unsafe configuration accepted")
		}
	}
	if failure(&textproto.Error{Code: 550, Msg: "private server text"}).Temporary {
		t.Fatal("permanent SMTP failure retried")
	}
	if !failure(&textproto.Error{Code: 451, Msg: "private server text"}).Temporary {
		t.Fatal("temporary SMTP failure lost retry")
	}
	if failure(x509.UnknownAuthorityError{}).Temporary {
		t.Fatal("invalid TLS authority retried")
	}
	path := filepath.Join(t.TempDir(), "password")
	os.WriteFile(path, []byte("secret"), 0644)
	if _, e := secret(path); !errors.Is(e, ErrConfig) {
		t.Fatal("world-readable secret accepted")
	}
}
func TestPermanentOutboxFailureDoesNotRetryOrChangeJob(t *testing.T) {
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	db, e := store.Open(ctx, dir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = db.ConfigureSMTP(ctx, true); e != nil {
		t.Fatal(e)
	}
	job, e := db.Enqueue(ctx, store.Job{Kind: domain.Inspect})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Claim(ctx); e != nil {
		t.Fatal(e)
	}
	if e = db.FinishExecution(ctx, job.ID, errors.New("synthetic failure")); e != nil {
		t.Fatal(e)
	}
	calls := 0
	sender := func(context.Context, Config, Message) (Result, error) {
		calls++
		return Result{}, &DeliveryError{Code: "SMTP_REJECTED", Temporary: false}
	}
	worked, e := deliverOne(ctx, db, Config{}, sender)
	if e != nil || !worked {
		t.Fatal(worked, e)
	}
	worked, e = deliverOne(ctx, db, Config{}, sender)
	if e != nil || worked || calls != 1 {
		t.Fatal("permanent failure retried", calls, e)
	}
	actual, e := db.GetJob(ctx, job.ID)
	if e != nil || actual.State != domain.Failed {
		t.Fatal("SMTP changed job integrity", actual, e)
	}
	inbox, e := db.ListNotifications(ctx, 0, 200)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, n := range inbox {
		if n.Code == "SMTP_DELIVERY_FAILED" {
			found = true
		}
		if strings.Contains(n.Message, "private server text") {
			t.Fatal("server text leaked")
		}
	}
	if !found {
		t.Fatal("delivery failure absent from inbox")
	}
}

func TestAcceptedMessageIsNeverRetriedAfterSessionCloseFailure(t *testing.T) {
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	db, e := store.Open(ctx, dir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = db.ConfigureSMTP(ctx, true); e != nil {
		t.Fatal(e)
	}
	job, e := db.Enqueue(ctx, store.Job{Kind: domain.Inspect})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Claim(ctx); e != nil {
		t.Fatal(e)
	}
	if e = db.FinishExecution(ctx, job.ID, errors.New("synthetic failure")); e != nil {
		t.Fatal(e)
	}
	calls := 0
	sender := func(context.Context, Config, Message) (Result, error) {
		calls++
		return Result{Accepted: true}, &DeliveryError{Code: "SMTP_SESSION_CLOSE_FAILED"}
	}
	if worked, e := deliverOne(ctx, db, Config{}, sender); e != nil || !worked {
		t.Fatal(worked, e)
	}
	if worked, e := deliverOne(ctx, db, Config{}, sender); e != nil || worked || calls != 1 {
		t.Fatal("accepted message replayed", calls, e)
	}
}
