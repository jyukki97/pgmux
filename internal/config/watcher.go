package config

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const debounceInterval = 1 * time.Second

// FileWatcher watches a config file for changes and triggers a callback.
// It watches the parent directory to handle K8s ConfigMap symlink swaps.
type FileWatcher struct {
	path     string
	fileName string
	onChange func()
	watcher  *fsnotify.Watcher

	mu      sync.Mutex
	stopped bool
	stopCh  chan struct{}
}

// NewFileWatcher creates a FileWatcher that monitors the given file path.
// The onChange callback is invoked (debounced) when the file is modified.
func NewFileWatcher(path string, onChange func()) (*FileWatcher, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create fsnotify watcher: %w", err)
	}

	return &FileWatcher{
		path:     absPath,
		fileName: filepath.Base(absPath),
		onChange: onChange,
		watcher:  watcher,
		stopCh:   make(chan struct{}),
	}, nil
}

// Start begins watching for file changes. It blocks until the context is
// cancelled or Stop is called.
func (fw *FileWatcher) Start(ctx context.Context) error {
	// Watch the parent directory to catch symlink swaps (K8s ConfigMap).
	dir := filepath.Dir(fw.path)
	if err := fw.watcher.Add(dir); err != nil {
		return fmt.Errorf("watch directory %s: %w", dir, err)
	}

	slog.Info("config file watcher started", "path", fw.path, "dir", dir)

	var debounceTimer *time.Timer

	for {
		select {
		case <-ctx.Done():
			fw.cleanup(debounceTimer)
			return nil
		case <-fw.stopCh:
			fw.cleanup(debounceTimer)
			return nil
		case event, ok := <-fw.watcher.Events:
			if !ok {
				return nil
			}
			if !fw.isTargetEvent(event) {
				continue
			}
			slog.Debug("config file event", "op", event.Op, "name", event.Name)

			// Debounce: reset timer on each qualifying event.
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(debounceInterval, fw.onChange)

		case err, ok := <-fw.watcher.Errors:
			if !ok {
				return nil
			}
			slog.Error("config file watcher error", "error", err)
		}
	}
}

// Stop terminates the file watcher.
func (fw *FileWatcher) Stop() {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.stopped {
		return
	}
	fw.stopped = true
	close(fw.stopCh)
	fw.watcher.Close()
}

// isTargetEvent returns true if the event is relevant to the watched file.
func (fw *FileWatcher) isTargetEvent(event fsnotify.Event) bool {
	// Only respond to write, create, and rename events.
	if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
		return false
	}

	eventBase := filepath.Base(event.Name)

	// Direct match: the config file itself was modified.
	if eventBase == fw.fileName {
		return true
	}

	// K8s ConfigMap symlink swap: when K8s updates a mounted ConfigMap,
	// it atomically swaps a "..data" symlink. Detect CREATE events on
	// symlink-like entries (prefixed with "..") in the watched directory.
	if event.Op&fsnotify.Create != 0 && strings.HasPrefix(eventBase, "..") {
		return true
	}

	return false
}

func (fw *FileWatcher) cleanup(timer *time.Timer) {
	if timer != nil {
		timer.Stop()
	}
}
