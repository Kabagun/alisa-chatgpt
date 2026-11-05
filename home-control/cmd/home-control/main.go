//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"smart-home-plugin/internal/app"
	"smart-home-plugin/internal/oauth"
	"smart-home-plugin/internal/provider"
	"smart-home-plugin/internal/providers/dreame"
)

type publicConfig struct {
	PublicOrigin string `json:"public_origin"`
}

func main() {
	if err := serve(); err != nil {
		log.Print("home_control_service_failed")
		os.Exit(1)
	}
}

func serve() error {
	if syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{}) != nil {
		return errors.New("process protection unavailable")
	}
	syscall.Umask(0077)
	configPath := flag.String("config", "public-config.json", "deployment public-origin configuration")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected argument")
	}
	host := os.Getenv("HOST")
	port, err := strconv.Atoi(os.Getenv("PORT"))
	dataDir := os.Getenv("APP_DATA_DIR")
	if err != nil || port < 1 || port > 65535 || host != "127.0.0.1" || !filepath.IsAbs(dataDir) {
		return errors.New("managed runtime configuration unavailable")
	}
	f, err := os.Open(*configPath)
	if err != nil {
		return errors.New("public configuration unavailable")
	}
	contents, readErr := io.ReadAll(io.LimitReader(f, 8193))
	if readErr != nil || len(contents) > 8192 {
		_ = f.Close()
		return errors.New("public configuration unavailable")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var config publicConfig
	err = decoder.Decode(&config)
	var extra any
	if err != nil || decoder.Decode(&extra) != io.EOF {
		_ = f.Close()
		return errors.New("public configuration invalid")
	}
	if f.Close() != nil {
		return errors.New("public configuration unavailable")
	}
	if _, err := oauth.CanonicalOrigin(config.PublicOrigin); err != nil {
		return errors.New("public configuration invalid")
	}
	dataDir, err = preparePrivateState(dataDir)
	if err != nil {
		return errors.New("private state migration unavailable")
	}
	executable, err := os.Executable()
	if err != nil {
		return errors.New("service location unavailable")
	}
	binDir := filepath.Dir(executable)
	dreameProvider, err := dreame.New(dreame.Config{
		ReaderPath:      filepath.Join(binDir, "dreame-reader"),
		ProvisionerPath: filepath.Join(binDir, "dreame-provision"),
		ProfilePath:     filepath.Join(filepath.Dir(binDir), "vendor-profile.json"),
		Timeout:         30 * time.Second,
	})
	if err != nil {
		return errors.New("provider configuration invalid")
	}
	handler, err := app.New(app.Config{
		PublicOrigin: config.PublicOrigin,
		DataDir:      dataDir,
		Providers:    []provider.Provider{dreameProvider},
		OnAuthDenial: func(code string) {
			log.Print("home_control_auth_denied reason=" + code)
		},
		AccessTTL:        5 * time.Minute,
		RefreshTTL:       30 * 24 * time.Hour,
		OperationTimeout: 40 * time.Second,
	})
	if err != nil {
		return errors.New("service storage configuration invalid")
	}
	defer handler.Close()
	server := &http.Server{
		Addr:              net.JoinHostPort(host, strconv.Itoa(port)),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      45 * time.Second,
		IdleTimeout:       45 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	closed := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
			deadline, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if server.Shutdown(deadline) != nil {
				_ = server.Close()
			}
		case <-closed:
		}
	}()
	log.Print("home_control_service_starting")
	err = server.ListenAndServe()
	close(closed)
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return errors.New("service listener failed")
}
