package ratelimit

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

type ConfigStore struct {
	ptr atomic.Pointer[Config]
}

func NewConfigStore(cfg *Config) *ConfigStore {
	s := &ConfigStore{}
	s.Set(cfg)
	return s
}

func (s *ConfigStore) Get() *Config {
	if s == nil {
		return nil
	}
	return s.ptr.Load()
}

func (s *ConfigStore) Set(cfg *Config) {
	if s == nil {
		return
	}
	s.ptr.Store(cfg)
}

func (s *ConfigStore) Watch(ctx context.Context, path string, onReload func(*Config)) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(filepath.Dir(path)); err != nil {
		_ = w.Close()
		return err
	}

	go func() {
		defer func() { _ = w.Close() }()

		debounce := time.NewTimer(time.Hour)
		if !debounce.Stop() {
			<-debounce.C
		}

		fallback := time.NewTicker(30 * time.Second)
		defer fallback.Stop()

		var lastMtime time.Time
		if info, err := os.Stat(path); err == nil {
			lastMtime = info.ModTime()
		}

		reload := func() {
			info, err := os.Stat(path)
			if err != nil {
				log.Warnf("ratelimit: stat config: %v", err)
				return
			}
			if info.ModTime().Equal(lastMtime) {
				return
			}

			cfg, err := LoadFromFile(path)
			if err != nil {
				// Don't advance lastMtime — partial writes (K8s ConfigMap atomic
				// rename, editor swap files) can momentarily produce malformed
				// YAML. Leaving lastMtime untouched lets the next event/tick
				// retry once the writer settles.
				log.Warnf("ratelimit: reload config: %v", err)
				return
			}
			lastMtime = info.ModTime()
			s.Set(cfg)
			if onReload != nil {
				onReload(cfg)
			}
			log.Infof("ratelimit: config reloaded (mtime=%s)", info.ModTime().Format(time.RFC3339))
		}

		trigger := func() {
			if !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
			debounce.Reset(500 * time.Millisecond)
		}

		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-w.Events:
				if !ok {
					return
				}
				trigger()
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Warnf("ratelimit: fsnotify error: %v", err)
			case <-debounce.C:
				reload()
			case <-fallback.C:
				reload()
			}
		}
	}()
	return nil
}
