package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gbaselite/executor"

	driver "github.com/go-sql-driver/mysql"
)

func TestOptionalTLSAcceptsEncryptedAndPlaintextClients(t *testing.T) {
	databaseServer, address, stop := startTLSTestServer(t, false)
	defer stop()

	pingMySQL(t, "root:123456@tcp("+address+")/?timeout=3s")
	pingMySQL(t, tlsTestDSN(t, address, databaseServer.TLSConfig))
}

func TestRequireSecureTransportRejectsPlaintextAndAcceptsTLS(t *testing.T) {
	databaseServer, address, stop := startTLSTestServer(t, true)
	defer stop()

	plaintext, err := sql.Open("mysql", "root:123456@tcp("+address+")/?timeout=3s")
	if err != nil {
		t.Fatal(err)
	}
	plaintext.SetMaxOpenConns(1)
	err = plaintext.Ping()
	_ = plaintext.Close()
	if err == nil || !strings.Contains(err.Error(), "3159") {
		t.Fatalf("plaintext connection error = %v, want MySQL 3159", err)
	}

	pingMySQL(t, tlsTestDSN(t, address, databaseServer.TLSConfig))
}

func startTLSTestServer(t *testing.T, requireSecureTransport bool) (*MySQLServer, string, func()) {
	t.Helper()
	engine, err := executor.Open(t.TempDir(), "root", "123456")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	databaseServer := &MySQLServer{
		Engine:                 engine,
		Logger:                 log.New(io.Discard, "", 0),
		TLSConfig:              tlsTestServerConfig(t),
		RequireSecureTransport: requireSecureTransport,
	}
	done := make(chan error, 1)
	go func() { done <- databaseServer.Serve(listener) }()
	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := databaseServer.Shutdown(ctx); err != nil {
			t.Errorf("shutdown TLS test server: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve TLS test server: %v", err)
		}
	}
	return databaseServer, listener.Addr().String(), stop
}

func pingMySQL(t *testing.T, dsn string) {
	t.Helper()
	client, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	client.SetMaxOpenConns(1)
	defer client.Close()
	if err := client.Ping(); err != nil {
		t.Fatal(err)
	}
}

func tlsTestDSN(t *testing.T, address string, serverConfig *tls.Config) string {
	t.Helper()
	name := fmt.Sprintf("gbaselite_test_%d", time.Now().UnixNano())
	clientConfig := &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	}
	if err := driver.RegisterTLSConfig(name, clientConfig); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { driver.DeregisterTLSConfig(name) })
	if len(serverConfig.Certificates) == 0 {
		t.Fatal("TLS test server has no certificate")
	}
	return "root:123456@tcp(" + address + ")/?timeout=3s&tls=" + name
}

func tlsTestServerConfig(t *testing.T) *tls.Config {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "GBaseLite test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	certPath := filepath.Join(directory, "server.crt")
	keyPath := filepath.Join(directory, "server.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	keyBytes := x509.MarshalPKCS1PrivateKey(privateKey)
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyBytes}), 0o600); err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
}
