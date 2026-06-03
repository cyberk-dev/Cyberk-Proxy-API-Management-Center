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

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	// Side-effect import: registers every built-in request/response translator
	// (claude→codex, openai→codex, etc.) into the default translator registry.
	// Without this, codex_executor's TranslateRequest falls through to the
	// no-op fallback and the raw Anthropic /v1/messages body gets sent to
	// chatgpt.com/backend-api/codex/responses, which rejects it. Stock
	// cmd/server/main.go does this via internal/translator; plugins use the
	// public sdk/translator/builtin alias.
	_ "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
	log "github.com/sirupsen/logrus"

	"github.com/cyberk/ratelimit-plugin/internal/contextbudget"
	"github.com/cyberk/ratelimit-plugin/internal/effortnormalize"
	"github.com/cyberk/ratelimit-plugin/internal/policy"
	"github.com/cyberk/ratelimit-plugin/internal/promptlog"
	"github.com/cyberk/ratelimit-plugin/internal/ratelimit"
	"github.com/cyberk/ratelimit-plugin/internal/usagepush"
	"github.com/cyberk/ratelimit-plugin/internal/usagestore"
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

	policyCfg, err := policy.LoadFromFile(absCfg)
	if err != nil {
		log.Warnf("policy: load failed, running with empty config: %v", err)
		policyCfg = &policy.Config{}
	}
	policyStore := policy.NewConfigStore(policyCfg)
	// strip_priority defaults on, so log unconditionally — operators need to know
	// priority fast-mode is being silently stripped even with no policy section.
	log.Infof("policy: strip_priority=%v block_service_tiers=%v",
		policyCfg.ShouldStripPriority(), policyCfg.BlockServiceTiers)

	cbCfg, err := contextbudget.LoadFromFile(absCfg)
	if err != nil {
		log.Warnf("context_budget: load failed, running disabled: %v", err)
		cbCfg = &contextbudget.Config{}
	}
	cbStore := contextbudget.NewConfigStore(cbCfg)
	// Session tracker bounds: 4096 sessions × ~40 bytes/entry ≈ <200 KiB.
	// 30-minute TTL trades off some cross-restart accuracy for memory.
	cbTracker := contextbudget.NewTracker(4096, 30*time.Minute)
	cbTracker.SetSoftBlockBurst(cbCfg.SoftBlockBurst())
	if cbCfg.Enabled() {
		log.Infof("context_budget: enabled (soft=%d hard=%d burst=%s)",
			cbCfg.Soft(), cbCfg.Hard(), cbCfg.SoftBlockBurst())
	}

	plogCfg, err := promptlog.LoadFromFile(absCfg)
	if err != nil {
		log.Warnf("promptlog: load failed, disabled: %v", err)
		plogCfg = &promptlog.Config{}
	}
	// Default the dir to <config-dir>/prompts when enabled with no explicit
	// path, mirroring how state defaults to <config-dir>/ratelimit-state.json.
	if plogCfg.IsEnabled() && !filepath.IsAbs(plogCfg.Dir) {
		plogCfg.Dir = filepath.Join(filepath.Dir(absCfg), plogCfg.Dir)
	}
	var plogWriter *promptlog.Writer
	var plogTemplates *promptlog.TemplateStore
	var plogIndex *promptlog.Index
	if plogCfg.IsEnabled() {
		plogTemplates, err = promptlog.NewTemplateStore(plogCfg.Dir)
		if err != nil {
			log.Warnf("promptlog: templates init failed, disabled: %v", err)
			plogTemplates = nil
		}
		// Cold-start scan: rebuilds the in-memory index from every
		// prompts-*.jsonl file. Logs files/entries/dur so operators can
		// spot regressions. On error we degrade gracefully — nil index
		// makes the read handlers fall back to scan-on-read (slower but
		// still correct).
		plogIndex, err = promptlog.NewIndex(plogCfg.Dir)
		if err != nil {
			log.Warnf("promptlog: index init failed (read path will scan disk): %v", err)
			plogIndex = nil
		}
		plogWriter, err = promptlog.NewWriter(plogCfg.Dir, plogCfg.QueueSize, plogTemplates, plogCfg.Templates, plogIndex)
		if err != nil {
			log.Warnf("promptlog: writer init failed, disabled: %v", err)
			plogCfg = &promptlog.Config{}
		} else {
			log.Infof("promptlog: enabled (dir=%s max_text=%d queue=%d templates=%v)",
				plogCfg.Dir, plogCfg.MaxTextBytes, plogCfg.QueueSize, plogCfg.Templates.Enabled)
		}
	}

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

	if err := policyStore.Watch(ctx, absCfg, func(c *policy.Config) {
		log.Infof("policy: config swapped (strip_priority=%v block_service_tiers=%v)",
			c.ShouldStripPriority(), c.BlockServiceTiers)
	}); err != nil {
		log.Warnf("policy: watcher disabled: %v", err)
	}

	if err := cbStore.Watch(ctx, absCfg, func(c *contextbudget.Config) {
		cbTracker.SetSoftBlockBurst(c.SoftBlockBurst())
		log.Infof("context_budget: config swapped (enabled=%v soft=%d hard=%d burst=%s)",
			c.Enabled(), c.Soft(), c.Hard(), c.SoftBlockBurst())
	}); err != nil {
		log.Warnf("context_budget: watcher disabled: %v", err)
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
				// Sweep stale soft-warning flags. Without this, sessions
				// that warned and went dormant past the tracker TTL stay
				// in the warnedSessions map forever (only active sessions
				// trigger the implicit cleanup via MarkWarnedIfFirst /
				// ClearWarning paths).
				cbTracker.SweepWarned()
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

	ustore := usagestore.New()
	ustore.RegisterPlugin()

	// NOTE: previously we registered `contextbudget.NewUsagePlugin` as
	// a coreusage.Plugin to capture token counts post-response. That
	// path is fundamentally racy: HandleUsage runs async off a queue
	// AFTER Gin has returned *gin.Context to its sync.Pool, by which
	// point our c.Set keys have been wiped by c.reset(). The middleware
	// now captures upstream usage synchronously via a ResponseWriter
	// wrapper (see internal/contextbudget/capture.go) inside the same
	// request goroutine, so this registration is no longer needed.

	// Promptlog runs first: it must observe *every* request, including those
	// rejected by policy/ratelimit, since rejected attempts are part of the
	// behavior we want to analyze. Policy runs next so blocked requests still
	// don't consume rate-limit budget. Context-budget runs LAST so the prompt
	// log captures the original (unmutated) body and rate-limit accounting
	// applies to the pre-reminder request.
	builder := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(absCfg).
		WithServerOptions(
			api.WithMiddleware(
				promptlog.Middleware(plogCfg, plogWriter),
				effortnormalize.Middleware(),
				policy.Middleware(policyStore),
				ratelimit.Middleware(store, limiter),
				contextbudget.Middleware(cbStore, cbTracker),
			),
			api.WithRouterConfigurator(func(engine *gin.Engine, _ *handlers.BaseAPIHandler, c *config.Config) {
				usagepush.Register(engine, c)
				usagestore.RegisterRoutes(engine, c, ustore, rlBridge{store})
				promptlog.RegisterReadHandlers(engine, c, plogCfg, plogTemplates, plogIndex)
			}),
		)

	wcfg, werr := weightedselector.LoadFromYAML(absCfg)
	if werr != nil {
		log.Fatalf("weighted selector: load codex_weights: %v", werr)
	}
	mgr := buildCoreManager(cfg, wcfg, store, limiter)
	builder = builder.WithCoreAuthManager(mgr)
	if wcfg.Enabled {
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
	if plogWriter != nil {
		plogWriter.Close()
		if dropped := plogWriter.Dropped(); dropped > 0 {
			log.Warnf("promptlog: %d entries dropped due to full queue", dropped)
		}
	}

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		log.Fatalf("cliproxy run: %v", runErr)
	}
}

// buildCoreManager builds a custom coreauth.Manager with the selector chain:
//
//	RateLimitSelector (outermost — WS frame enforcement)
//	 └─ SessionAffinitySelector (optional)
//	     └─ WeightedSelector (when codex_weights enabled)
//	         ├─ codex → SWRR by plan_type weight
//	         └─ others → RoundRobin / FillFirst (base)
//
// RateLimitSelector wraps the entire chain to enforce per-frame rate limits on
// downstream WebSocket connections. HTTP requests are rate-limited by the Gin
// middleware instead, keeping the two paths disjoint and sharing the same
// Limiter buckets for aggregate counting.
func buildCoreManager(cfg *config.Config, wcfg weightedselector.Config, rlStore *ratelimit.ConfigStore, rlLimiter *ratelimit.Limiter) *coreauth.Manager {
	tokenStore := sdkAuth.GetTokenStore()
	if setter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok && cfg != nil {
		setter.SetBaseDir(cfg.AuthDir)
	}

	strategy := ""
	sessionAffinity := false
	sessionAffinityTTL := time.Hour
	if cfg != nil {
		strategy = strings.ToLower(strings.TrimSpace(cfg.Routing.Strategy))
		sessionAffinity = cfg.Routing.SessionAffinity
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

	var selector coreauth.Selector
	if wcfg.Enabled {
		selector = weightedselector.New(base, wcfg)
	} else {
		selector = base
	}
	if sessionAffinity {
		selector = coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
			Fallback: selector,
			TTL:      sessionAffinityTTL,
		})
	}
	selector = ratelimit.NewRateLimitSelector(selector, rlStore, rlLimiter)
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

// rlBridge adapts the hot-reloadable *ratelimit.ConfigStore to the
// usagestore.RateLimitResolver interface so the user-detail endpoint can
// report rate-limit panel data without importing the ratelimit package.
type rlBridge struct {
	store *ratelimit.ConfigStore
}

func (b rlBridge) Resolve(apiKey, model string) (int, time.Duration, bool) {
	if b.store == nil {
		return 0, 0, false
	}
	cfg := b.store.Get()
	if cfg == nil {
		return 0, 0, false
	}
	return cfg.Resolve(apiKey, model)
}
