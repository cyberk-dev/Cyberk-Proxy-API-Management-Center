package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"

	"github.com/cyberk/ratelimit-plugin/internal/ratelimit"
	"github.com/cyberk/ratelimit-plugin/internal/weightedselector"
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

	builder := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(absCfg).
		WithServerOptions(api.WithMiddleware(mw))

	wcfg, werr := weightedselector.LoadFromYAML(absCfg)
	if werr != nil {
		log.Fatalf("weighted selector: load codex_weights: %v", werr)
	}
	if wcfg.Enabled {
		mgr := buildWeightedCoreManager(cfg, wcfg)
		builder = builder.WithCoreAuthManager(mgr)
		log.Infof("weighted selector: enabled for codex (entries=%d)", len(wcfg.Weights))
	}

	svc, err := builder.Build()
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

// buildWeightedCoreManager mirrors the SDK's default wiring in
// sdk/cliproxy/builder.go:204-240, but inserts our weighted selector between
// session-affinity and the round-robin/fill-first base. Composition order:
//
//	SessionAffinitySelector (optional, outer)
//	 └─ WeightedSelector
//	     ├─ codex → SWRR by plan_type weight
//	     └─ others → RoundRobin / FillFirst (base)
//
// Rationale: when session affinity is enabled the operator wants sticky
// routing to trump everything else — a returning conversation must land on its
// original auth regardless of weights. Weighted routing kicks in whenever SA
// delegates to its fallback: (a) on cache-miss for new conversations, AND
// (b) on cache-hit when the cached auth has become unavailable and SA
// re-selects (see SDK selector.go:498-512 — it calls s.fallback.Pick with the
// full auth list, which lands here).
func buildWeightedCoreManager(cfg *config.Config, wcfg weightedselector.Config) *coreauth.Manager {
	tokenStore := sdkAuth.GetTokenStore()
	if setter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok && cfg != nil {
		setter.SetBaseDir(cfg.AuthDir)
	}

	strategy := ""
	sessionAffinity := false
	sessionAffinityTTL := time.Hour
	if cfg != nil {
		strategy = strings.ToLower(strings.TrimSpace(cfg.Routing.Strategy))
		sessionAffinity = cfg.Routing.ClaudeCodeSessionAffinity || cfg.Routing.SessionAffinity
		if ttlStr := strings.TrimSpace(cfg.Routing.SessionAffinityTTL); ttlStr != "" {
			if parsed, err := time.ParseDuration(ttlStr); err == nil && parsed > 0 {
				sessionAffinityTTL = parsed
			}
		}
	}

	var base coreauth.Selector
	switch strategy {
	case "fill-first", "fillfirst", "ff":
		base = &coreauth.FillFirstSelector{}
	default:
		base = &coreauth.RoundRobinSelector{}
	}

	var selector coreauth.Selector = weightedselector.New(base, wcfg)
	if sessionAffinity {
		selector = coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
			Fallback: selector,
			TTL:      sessionAffinityTTL,
		})
	}
	return coreauth.NewManager(tokenStore, selector, nil)
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
