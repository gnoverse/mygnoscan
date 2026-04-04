package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

//go:embed frontend
var frontendFS embed.FS

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listenAddr  = flag.String("listen", ":8888", "listen address")
		indexerURL  = flag.String("indexer", "https://indexer.gno.land/graphql/query", "tx-indexer GraphQL endpoint")
		dbPath      = flag.String("db", "mygnoscan.db", "SQLite database path")
		syncOnStart = flag.Bool("sync", true, "sync data from indexer on start")
	)
	flag.Parse()

	// Initialize database
	db, err := NewDB(*dbPath)
	if err != nil {
		return fmt.Errorf("init db: %w", err)
	}
	defer db.Close()

	// Initialize indexer client
	client := NewIndexerClient(*indexerURL)

	// Initialize analyzer
	analyzer := NewAnalyzer(db)

	// Initialize syncer
	syncer := NewSyncer(client, db, analyzer)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Sync data from indexer
	if *syncOnStart {
		go func() {
			log.Println("starting initial sync...")
			if err := syncer.SyncAll(ctx); err != nil {
				log.Printf("sync error: %v", err)
			}
			log.Println("initial sync complete")

			// Periodic re-sync every 30s
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := syncer.SyncAll(ctx); err != nil {
						log.Printf("sync error: %v", err)
					}
				}
			}
		}()
	}

	// Set up API routes
	api := NewAPI(db, client, analyzer)
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("GET /api/stats", api.HandleStats)
	mux.HandleFunc("GET /api/realms", api.HandleRealms)
	mux.HandleFunc("GET /api/realm/{path...}", api.HandleRealm)
	mux.HandleFunc("GET /api/packages", api.HandlePackages)
	mux.HandleFunc("GET /api/tx/{hash}", api.HandleTx)
	mux.HandleFunc("GET /api/txs", api.HandleTxs)
	mux.HandleFunc("GET /api/address/{addr}", api.HandleAddress)
	mux.HandleFunc("GET /api/search", api.HandleSearch)
	mux.HandleFunc("GET /api/deps/{path...}", api.HandleDeps)
	mux.HandleFunc("GET /api/events/{path...}", api.HandleEvents)
	mux.HandleFunc("GET /api/blocks", api.HandleBlocks)
	mux.HandleFunc("GET /api/block/{height}", api.HandleBlock)
	mux.HandleFunc("GET /api/validators", api.HandleValidators)
	mux.HandleFunc("GET /api/tokens", api.HandleTokens)
	mux.HandleFunc("GET /api/accounts", api.HandleAccounts)
	mux.HandleFunc("GET /api/govdao", api.HandleGovDAO)

	// Frontend: SPA handler serves index.html for all non-API routes
	frontendSub, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		return fmt.Errorf("frontend fs: %w", err)
	}
	staticFS := http.FileServer(http.FS(frontendSub))
	indexHTML, err := fs.ReadFile(frontendFS, "frontend/index.html")
	if err != nil {
		return fmt.Errorf("read index.html: %w", err)
	}
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// Try serving static file first (css, js, images)
		if r.URL.Path != "/" {
			f, err := frontendSub.Open(r.URL.Path[1:]) // strip leading /
			if err == nil {
				f.Close()
				staticFS.ServeHTTP(w, r)
				return
			}
		}
		// Serve index.html for all other routes (SPA)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})

	srv := &http.Server{
		Addr:         *listenAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("shutting down...")
		cancel()
		srv.Shutdown(context.Background())
	}()

	log.Printf("mygnoscan listening on %s", *listenAddr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}
