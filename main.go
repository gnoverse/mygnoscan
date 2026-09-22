package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/moul/mygnoscan/pkg/analyzer"
	"github.com/moul/mygnoscan/pkg/config"
	"github.com/moul/mygnoscan/pkg/httpapi"
	"github.com/moul/mygnoscan/pkg/indexer"
	"github.com/moul/mygnoscan/pkg/store"
	"github.com/moul/mygnoscan/pkg/syncer"
	"github.com/moul/mygnoscan/pkg/web"
)

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
		// Block backfill is the one sync phase that can pull hundreds of
		// megabytes per network (~130 bytes/block, ~430MB at mainnet's 3.3M
		// blocks), so it is the one phase an operator must be able to bound.
		// 90 days is the dashboards' default window, so the default depth is
		// exactly what the default view shows.
		blockHistoryDays = flag.Int("block-history-days", 90,
			"days of block history to backfill (0 = full chain history, negative = do not store blocks at all)")
		// Off unless an operator asks for it: every deployment shares the same
		// embedded frontend, so a hardcoded tag would make every one of them
		// report to somebody else's analytics account.
		analyticsScript = flag.String("analytics-script", "",
			"URL of an analytics script to load in the frontend, e.g. https://scripts.simpleanalyticscdn.com/latest.js (empty = none)")
		// Realm screenshots. Empty means the explorer draws no pictures of
		// realms at all, which is the right default: pointing at a capture
		// service that is not there would put a broken tile on every row.
		gnoshotURL = flag.String("gnoshot", "",
			"base URL of a gnoshot capture service, e.g. http://127.0.0.1:8890 (empty = no realm screenshots)")
	)
	flag.Parse()

	// Initialize database
	db, err := store.NewDB(*dbPath)
	if err != nil {
		return fmt.Errorf("init db: %w", err)
	}
	defer db.Close()

	// Load config
	cfg, cfgSource, err := config.ResolveConfig(*configPath, *networkFlag, *indexerFlag, *rpcFlag)
	if err != nil {
		return err
	}
	log.Printf("networks %v (from %s)", cfg.IDs(), cfgSource)
	switch {
	case *blockHistoryDays < 0:
		log.Printf("block history: disabled (-block-history-days=%d); block charts will be empty", *blockHistoryDays)
	case *blockHistoryDays == 0:
		log.Printf("block history: full chain (-block-history-days=0)")
	default:
		log.Printf("block history: %d days (-block-history-days)", *blockHistoryDays)
	}

	// Rows outlive a network being retired from the config, so tell the database
	// which ones still count before anything reads an all-networks total.
	db.SetConfiguredNetworks(cfg.Networks)

	// Create per-network clients. The sync loop gets its own, on a budget sized
	// for catching up on history rather than for answering a page.
	clients := make(map[string]*indexer.Client)
	syncClients := make(map[string]*indexer.Client)
	for _, n := range cfg.Networks {
		clients[n.ID] = indexer.NewClient(n.Indexers()...)
		syncClients[n.ID] = indexer.NewSyncClient(n.Indexers()...)
	}

	// Initialize analyzer
	analyzer := analyzer.NewAnalyzer(db)

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

	// Written by the sync goroutines below, read by the sanity endpoint.
	syncHealth := syncer.NewRegistry()

	// Sync data from indexer (one goroutine per network)
	//
	// Every pass records its outcome, success or failure, so the sanity page
	// can say whether we are managing to read each chain. The log line alone
	// was not enough: a pass that fails every time for a day looks, from every
	// surface the explorer offers, exactly like one that never fails.
	if *syncOnStart {
		for _, n := range cfg.Networks {
			go func(net config.NetworkConfig) {
				sy := syncer.NewSyncer(syncClients[net.ID], db, analyzer, net.ID)
				sy.SetBlockHistoryDays(*blockHistoryDays)
				log.Printf("[%s] starting initial sync...", net.ID)
				err := sy.SyncAll(ctx)
				syncHealth.Record(net.ID, err)
				if err != nil {
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
						err := sy.SyncAll(ctx)
						syncHealth.Record(net.ID, err)
						if err != nil {
							log.Printf("[%s] sync error: %v", net.ID, err)
						}
					}
				}
			}(n)
		}
	}

	// Keep the rollups warm.
	//
	// The aggregates behind the gas page, the bank page and the active-address
	// series scale with the chain and had reached 14, 5 and 16 seconds on
	// sapphire, against a 30-second write timeout. None can be indexed away —
	// attributing gas per realm means touching every call, and counting distinct
	// active addresses means touching every call, deploy and send — so they are
	// precomputed here instead of per request.
	//
	// Every five minutes, not every sync pass: the recompute takes the write
	// lock for a few seconds, and nothing here is a number a reader watches tick.
	// The gas and bank responses carry computed_at so the page can say how fresh
	// they are; the active-address series instead reads everything newer than
	// the build live, because a lagging newest bucket would disagree with the
	// live feed beside it.
	go func() {
		refresh := func() {
			start := time.Now()
			if err := db.RefreshRollups(); err != nil {
				log.Printf("rollups: %v", err)
				return
			}
			log.Printf("rollups refreshed in %s", time.Since(start).Round(time.Millisecond))
		}
		// Wait out the startup ANALYZE before the first build. Both take the
		// write lock, and racing it loses the whole refresh to SQLITE_BUSY —
		// which is silent, because the read path just falls back to computing
		// live and the page stays slow with nothing obviously broken.
		db.WaitBackground()
		refresh() // once at startup, so the first visitor is not the one who pays

		ticker := time.NewTicker(store.RollupInterval)
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
	api := httpapi.NewAPI(db, clients, cfg.Networks, analyzer)
	api.SetSyncHealth(syncHealth)
	api.SetShotUpstream(*gnoshotURL)
	if api.ShotsEnabled() {
		log.Printf("screenshots: /api/shot proxies %s", *gnoshotURL)
	}

	// A network pairs an indexer with an RPC, and nothing checked they serve the
	// same chain. Verify before serving rather than after someone reads a
	// balance from one chain beside history from another.
	//
	// Re-checked periodically because an endpoint can be repointed under a
	// running process — which is exactly what a mainnet launch on an existing
	// hostname does.
	// Account balances, on their own slower timer, and only for networks whose
	// RPC has been verified above.
	//
	// Same reason as the rollups: too slow to do per request. Different reason
	// for the interval, since this one is traffic against somebody else's node
	// rather than work on our own database. See pkg/httpapi/balances.go.
	go func() {
		db.WaitBackground()
		api.RunBalanceSweeper(ctx)
	}()

	go func() {
		check := func() {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			api.VerifyRPCChains(ctx)
		}
		check()

		ticker := time.NewTicker(httpapi.RPCChainRecheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// shots tells the frontend whether to put any realm <img> on the page.
		// It asks once and decides for the whole session, so a deployment
		// without a capture service never requests an image it cannot get.
		fmt.Fprintf(w, `{"git_hash":%q,"build_time":%q,"shots":%t}`, gitHash, buildTime, api.ShotsEnabled())
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
		httpapi.JSONResponse(w, nets)
	})
	api.RegisterRoutes(mux)

	// SSE live feed
	httpapi.InitLiveFeeds(cfg.Networks, clients)
	mux.HandleFunc("GET /api/live", httpapi.LiveFeedHandler())

	// Frontend: SPA handler serves index.html for all non-API routes
	frontend, err := web.Handler(web.Options{AnalyticsScript: *analyticsScript, Shots: api.ShotsEnabled()})
	if err != nil {
		return err
	}
	if *analyticsScript != "" {
		log.Printf("analytics: frontend loads %s", *analyticsScript)
	}
	mux.HandleFunc("GET /", frontend)

	// Cache outermost, so a hit costs nothing beyond the network-name check —
	// and compression *inside* it, so what the cache stores is already
	// compressed and a hit does not re-gzip 3.5 MB of JSON per reader. The
	// cache key carries the negotiated encoding to keep those two facts
	// consistent (see cacheKey).
	cache := httpapi.NewResponseCache(httpapi.CacheTTL)
	handler := httpapi.WithResponseCache(cache,
		httpapi.RejectUnknownNetwork(cfg.Networks,
			httpapi.WithCompression(mux)))

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
