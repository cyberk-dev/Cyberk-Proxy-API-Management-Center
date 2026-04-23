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
			lastMtime = info.ModTime()

			cfg, err := LoadFromFile(path)
			if err != nil {
				log.Warnf("ratelimit: reload config: %v", err)
				return
			}
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
