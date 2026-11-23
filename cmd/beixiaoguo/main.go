package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/beilunyang/openhongguo/adapters"
	"github.com/beilunyang/openhongguo/adapters/memstore"
	"github.com/beilunyang/openhongguo/sdk"

	"github.com/xiaobei206/beixiaoguo-web/internal/api"
)

var version = "0.1.6"

func main() {
	logger := log.New(os.Stdout, "[beixiaoguo] ", log.LstdFlags|log.Lmicroseconds)

	// Keep the anonymous SDK profile in RAM. No profile, database, cache or
	// application log is written to the N1 flash storage by default.
	client, err := adapters.NewClient("", sdk.Options{
		ProfileStore:  memstore.New(),
		RetryAttempts: 2,
		Logger:        logger,
	})
	if err != nil {
		logger.Fatalf("initialize SDK client: %v", err)
	}

	addr := strings.TrimSpace(os.Getenv("BEIXIAOGUO_ADDR"))
	if addr == "" {
		addr = "0.0.0.0:8780"
	}

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           api.New(client, version, logger),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Printf("贝小果 %s listening on http://%s", version, addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("HTTP server: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Printf("graceful shutdown: %v", err)
		_ = httpServer.Close()
	}
	logger.Print("stopped")
}
