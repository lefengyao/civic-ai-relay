package config

import "sync"

// Source holds the currently active settings behind a lock so administrative
// updates can take effect without a process restart. The relay service reads
// through Get on every request; the admin handler writes through Set after a
// validated config save.
type Source struct {
	mu       sync.RWMutex
	settings Settings
}

func NewSource(settings Settings) *Source {
	return &Source{settings: settings}
}

func (src *Source) Get() Settings {
	if src == nil {
		return Settings{}
	}
	src.mu.RLock()
	defer src.mu.RUnlock()
	return src.settings
}

func (src *Source) Set(settings Settings) {
	if src == nil {
		return
	}
	src.mu.Lock()
	src.settings = settings
	src.mu.Unlock()
}
