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

var gitHash = "dev"       // set via -ldflags at build time
var buildTime = "unknown" // set via -ldflags at build time

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listenAddr  = flag.String("listen", ":8888", "listen address")
		configPath  = flag.String("config", "", "config file path (JSON)")
		networkFlag = flag.String("network", "", "single network ID (overrides config)")
		indexerFlag = flag.String("indexer", "", "single network indexer URL (overrides config)")
		rpcFlag     = flag.String("rpc", "", "single network RPC URL")
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

	// Load config
	cfg, cfgSource, err := ResolveConfig(*configPath, *networkFlag, *indexerFlag, *rpcFlag)
	if err != nil {
		return err
	}
	log.Printf("networks %v (from %s)", cfg.IDs(), cfgSource)

	// Rows outlive a network being retired from the config, so tell the database
	// which ones still count before anything reads an all-networks total.
	db.SetConfiguredNetworks(cfg.Networks)

	// Create per-network clients. The sync loop gets its own, on a budget sized
	// for catching up on history rather than for answering a page.
	clients := make(map[string]*IndexerClient)
	syncClients := make(map[string]*IndexerClient)
	for _, n := range cfg.Networks {
		clients[n.ID] = NewIndexerClient(n.IndexerURL)
		syncClients[n.ID] = NewSyncIndexerClient(n.IndexerURL)
	}

	// Initialize analyzer
	analyzer := NewAnalyzer(db)

	// Recompute dependency edges when the extractor has changed. Reads only
	// stored source, so it costs nothing on the network and is a no-op once the
	// current version is recorded. Errors are logged, not fatal: a stale
	// dependency graph is not a reason to refuse to start.
	go func() {
		if err := analyzer.ReextractDependencies(); err != nil {
			log.Printf("re-extract dependencies: %v", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Sync data from indexer (one goroutine per network)
	if *syncOnStart {
		for _, n := range cfg.Networks {
			go func(net NetworkConfig) {
				syncer := NewSyncer(syncClients[net.ID], db, analyzer, net.ID)
				log.Printf("[%s] starting initial sync...", net.ID)
				if err := syncer.SyncAll(ctx); err != nil {
					log.Printf("[%s] sync error: %v", net.ID, err)
				}
				log.Printf("[%s] initial sync complete", net.ID)

				ticker := time.NewTicker(30 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						if err := syncer.SyncAll(ctx); err != nil {
							log.Printf("[%s] sync error: %v", net.ID, err)
						}
					}
				}
			}(n)
		}
	}

	// Keep the gas rollups warm.
	//
	// The two aggregates behind the gas page scale with the chain and had
	// reached 14 seconds on sapphire, against a 30-second write timeout. They
	// cannot be indexed away — attributing gas per realm means touching every
	// call — so they are precomputed here instead of per request.
	//
	// Every five minutes, not every sync pass: these are all-time totals, so
	// five minutes of staleness costs nothing a reader would notice, and the
	// recompute takes the write lock for a few seconds. The response carries
	// computed_at so the page can say how fresh it is rather than implying live.
	go func() {
		refresh := func() {
			start := time.Now()
			if err := db.RefreshGasRollups(); err != nil {
				log.Printf("gas rollup: %v", err)
				return
			}
			log.Printf("gas rollup refreshed in %s", time.Since(start).Round(time.Millisecond))
		}
		// Wait out the startup ANALYZE before the first build. Both take the
		// write lock, and racing it loses the whole refresh to SQLITE_BUSY —
		// which is silent, because the read path just falls back to computing
		// live and the page stays slow with nothing obviously broken.
		db.WaitBackground()
		refresh() // once at startup, so the first visitor is not the one who pays

		ticker := time.NewTicker(gasRollupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()

	// Set up API routes
	api := NewAPI(db, clients, cfg.Networks, analyzer)
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"git_hash":%q,"build_time":%q}`, gitHash, buildTime)
	})
	mux.HandleFunc("GET /api/networks", func(w http.ResponseWriter, r *http.Request) {
		type netInfo struct {
			ID      string `json:"id"`
			Indexer string `json:"indexer,omitempty"`
			RPC     string `json:"rpc,omitempty"`
		}
		var nets []netInfo
		for _, n := range cfg.Networks {
			nets = append(nets, netInfo{ID: n.ID, Indexer: n.IndexerURL, RPC: n.RPCURL})
		}
		jsonResponse(w, nets)
	})
	api.RegisterRoutes(mux)

	// SSE live feed
	initLiveFeeds(cfg.Networks, clients)
	mux.HandleFunc("GET /api/live", liveFeedHandler())

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

	// Cache first, so a hit costs nothing beyond the network-name check.
	cache := newResponseCache(cacheTTL)
	handler := withResponseCache(cache, rejectUnknownNetwork(cfg.Networks, mux))

	srv := &http.Server{
		Addr:         *listenAddr,
		Handler:      handler,
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

	log.Printf("mygnoscan listening on %s (networks: %v)", *listenAddr, cfg.IDs())
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}
