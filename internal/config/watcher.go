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
	readyCh chan struct{} // closed when watcher is actively monitoring
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
		readyCh:  make(chan struct{}),
	}, nil
}

// Ready returns a channel that is closed when the watcher is actively monitoring
// the target directory. Callers can use this to synchronize with watcher startup:
//
//	go func() { _ = fw.Start(ctx) }()
//	<-fw.Ready()
func (fw *FileWatcher) Ready() <-chan struct{} {
	return fw.readyCh
}

// Start begins watching for file changes. It blocks until the context is
// cancelled or Stop is called. The directory watch is registered before
// signalling readiness, so no events can be lost after Ready() returns.
func (fw *FileWatcher) Start(ctx context.Context) error {
	// Watch the parent directory to catch symlink swaps (K8s ConfigMap).
	dir := filepath.Dir(fw.path)
	if err := fw.watcher.Add(dir); err != nil {
		return fmt.Errorf("watch directory %s: %w", dir, err)
	}

	slog.Info("config file watcher started", "path", fw.path, "dir", dir)

	// Signal that the directory watch is armed.
	close(fw.readyCh)

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
	eventBase := filepath.Base(event.Name)

	// Direct match: the config file itself was modified.
	if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) != 0 && eventBase == fw.fileName {
		return true
	}

	// K8s ConfigMap symlink swap: when K8s updates a mounted ConfigMap,
	// it atomically swaps a "..data" symlink via os.Rename. Depending on
	// the OS/filesystem, this produces CREATE, RENAME, or REMOVE events.
	// Accept any mutation event on ".." prefixed entries.
	if event.Op != 0 && strings.HasPrefix(eventBase, "..") {
		return true
	}

	return false
}

func (fw *FileWatcher) cleanup(timer *time.Timer) {
	if timer != nil {
		timer.Stop()
	}
}
