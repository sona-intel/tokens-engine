package config

import (
	"errors"
	"fmt"
	"time"
)

// HolderMap is the holder map: bubbles for the largest holders and links between holders that sent each other the token
type HolderMap struct {
	// WindowDays is how far back the links read transfers
	WindowDays int
	// PageBudget is the most HyperSync requests one map may spend
	PageBudget int
	// FreshFor is how long built links are served before a new build
	FreshFor time.Duration
	// KeepFor is how long a stored map stays before it is pruned
	KeepFor time.Duration
	// QueueMax is the most maps that may wait for a build
	QueueMax int
	// RetryFailedAfter is how long a map whose build failed waits before it is built again
	RetryFailedAfter time.Duration
}

type holderMapFile struct {
	WindowDays       int           `yaml:"window_days"`
	PageBudget       int           `yaml:"page_budget"`
	FreshFor         time.Duration `yaml:"fresh_for"`
	KeepFor          time.Duration `yaml:"keep_for"`
	QueueMax         int           `yaml:"queue_max"`
	RetryFailedAfter time.Duration `yaml:"retry_failed_after"`
}

func (x holderMapFile) build() HolderMap { return HolderMap(x) }

func (x holderMapFile) validate() error {
	switch {
	case x.WindowDays < 1 || x.WindowDays > 90:
		return fmt.Errorf("holder_map.window_days must be 1 to 90, got %d", x.WindowDays)
	case x.PageBudget < 1 || x.PageBudget > 20:
		return fmt.Errorf("holder_map.page_budget must be 1 to 20, got %d", x.PageBudget)
	case x.FreshFor < time.Minute || x.KeepFor < x.FreshFor:
		return errors.New("holder_map.fresh_for must be at least 1m and holder_map.keep_for at least fresh_for")
	case x.QueueMax < 1 || x.QueueMax > 1000:
		return fmt.Errorf("holder_map.queue_max must be 1 to 1000, got %d", x.QueueMax)
	case x.RetryFailedAfter < time.Minute:
		return errors.New("holder_map.retry_failed_after must be at least 1m")
	}
	return nil
}
