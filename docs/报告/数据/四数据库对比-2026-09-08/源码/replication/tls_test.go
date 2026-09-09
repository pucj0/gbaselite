package replication

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"github.com/hashicorp/raft"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMutualTLSReplicationTransport(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test replication"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, IsCA: true, BasicConstraintsValid: true, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	cert, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
	stream, err := openStream(Options{Bind: "127.0.0.1:0", Advertise: "127.0.0.1:1", TLSCert: certFile, TLSKey: keyFile, TLSCA: certFile})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	address := raft.ServerAddress(stream.Listener.Addr().String())
	accept := func() chan error {
		done := make(chan error, 1)
		go func() {
			c, err := stream.Accept()
			if err != nil {
				done <- err
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(2 * time.Second))
			err = c.(*tls.Conn).Handshake()
			done <- err
		}()
		return done
	}
	done := accept()
	c, err := stream.Dial(address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	c.Close()
	done = accept()
	untrusted, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", string(address), &tls.Config{RootCAs: stream.config.RootCAs, MinVersion: tls.VersionTLS12})
	if untrusted != nil {
		defer untrusted.Close()
	}
	if serverErr := <-done; serverErr == nil {
		t.Fatalf("client without certificate accepted; dial=%v", err)
	}
	if _, err = openStream(Options{Bind: "0.0.0.0:0", Advertise: "127.0.0.1:1"}); err == nil {
		t.Fatal("remote plaintext listener accepted")
	}
}
