package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"

	"github.com/cyberk/ratelimit-plugin/internal/ratelimit"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	statePath := flag.String("state", "", "path to persisted counter state (default: <config-dir>/ratelimit-state.json)")
	saveInterval := flag.Duration("state-interval", 5*time.Second, "interval to flush counter state to disk")
	flag.Parse()

	absCfg, err := filepath.Abs(*cfgPath)
	if err != nil {
		log.Fatalf("resolve config path: %v", err)
	}

	if *statePath == "" {
		*statePath = filepath.Join(filepath.Dir(absCfg), "ratelimit-state.json")
	}

	cfg, err := config.LoadConfig(absCfg)
	if err != nil {
		log.Fatalf("load cliproxy config: %v", err)
	}

	rlCfg, err := ratelimit.LoadFromFile(absCfg)
	if err != nil {
		log.Warnf("ratelimit: load failed, running with empty config: %v", err)
		rlCfg = &ratelimit.Config{Models: map[string]ratelimit.ModelConfig{}}
	}
	store := ratelimit.NewConfigStore(rlCfg)

	limiter := ratelimit.NewLimiter()

	maxWindow := maxWindowOf(rlCfg)
	if err := limiter.Load(*statePath, maxWindow); err != nil {
		log.Warnf("ratelimit: load state: %v", err)
	} else if rlCfg.Enabled() {
		log.Infof("ratelimit: loaded state from %s (keys=%d)", *statePath, limiter.Size())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := store.Watch(ctx, absCfg, func(c *ratelimit.Config) {
		log.Infof("ratelimit: config swapped (top=%dreq/%s models=%d)", c.Requests, c.Window, len(c.Models))
	}); err != nil {
		log.Warnf("ratelimit: watcher disabled: %v", err)
	}

	var persistWG sync.WaitGroup
	persistWG.Add(1)
	go func() {
		defer persistWG.Done()
		ticker := time.NewTicker(*saveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				if err := limiter.Save(*statePath); err != nil {
					log.Warnf("ratelimit: final save: %v", err)
				}
				return
			case <-ticker.C:
				if !limiter.TakeDirty() {
					continue
				}
				if err := limiter.Save(*statePath); err != nil {
					log.Warnf("ratelimit: save state: %v", err)
				}
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				limiter.PruneIdle()
			}
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Info("ratelimit: shutdown signal received")
		cancel()
	}()

	mw := ratelimit.Middleware(store, limiter)

	svc, err := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(absCfg).
		WithServerOptions(api.WithMiddleware(mw)).
		Build()
	if err != nil {
		log.Fatalf("build cliproxy service: %v", err)
	}

	log.Infof("ratelimit-plugin starting (config=%s state=%s)", absCfg, *statePath)

	runErr := svc.Run(ctx)
	cancel()
	persistWG.Wait()

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		log.Fatalf("cliproxy run: %v", runErr)
	}
}

func maxWindowOf(cfg *ratelimit.Config) time.Duration {
	if cfg == nil {
		return 24 * time.Hour
	}
	max := cfg.Window
	for _, m := range cfg.Models {
		if m.Window != nil && *m.Window > max {
			max = *m.Window
		}
	}
	if max <= 0 {
		return 24 * time.Hour
	}
	return max
}
