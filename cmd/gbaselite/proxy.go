package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"gbaselite/config"
	"gbaselite/failover"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func runProxy(args []string) error {
	flags := flag.NewFlagSet("proxy", flag.ContinueOnError)
	configPath := flags.String("config", "config.yaml", "credentials configuration")
	listen := flags.String("listen", "127.0.0.1:3307", "frontend address")
	peers := flags.String("peers", "", "n1=sql-host:port,n2=sql-host:port,n3=sql-host:port")
	ca := flags.String("tls-ca", "", "CA PEM for backend SQL discovery")
	limit := flags.Int("max-connections", 64, "maximum routed connections")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	options := failover.Options{Username: cfg.Auth.Username, Password: cfg.Auth.Password, MaxConnections: *limit}
	for _, entry := range strings.Split(*peers, ",") {
		pair := strings.SplitN(strings.TrimSpace(entry), "=", 2)
		if len(pair) != 2 {
			return fmt.Errorf("--peers requires three node_id=SQL_address entries")
		}
		options.Peers = append(options.Peers, failover.Peer{ID: strings.TrimSpace(pair[0]), Address: strings.TrimSpace(pair[1])})
	}
	if *ca != "" {
		pem, err := os.ReadFile(*ca)
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("invalid SQL discovery CA")
		}
		options.TLS = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	}
	router, err := failover.New(options)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return router.Serve(ctx, listener)
}
