package config

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileWatcher_Modification(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(cfgFile, []byte("initial"), 0644); err != nil {
		t.Fatal(err)
	}

	var called atomic.Int32
	fw, err := NewFileWatcher(cfgFile, func() {
		called.Add(1)
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := fw.Start(ctx); err != nil {
			t.Errorf("Start() error: %v", err)
		}
	}()

	// Allow watcher to initialize.
	time.Sleep(100 * time.Millisecond)

	// Modify the file.
	if err := os.WriteFile(cfgFile, []byte("modified"), 0644); err != nil {
		t.Fatal(err)
	}

	// Wait for debounce interval + buffer.
	time.Sleep(debounceInterval + 500*time.Millisecond)

	if got := called.Load(); got != 1 {
		t.Errorf("callback called %d times, want 1", got)
	}

	fw.Stop()
}

func TestFileWatcher_Debounce(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(cfgFile, []byte("initial"), 0644); err != nil {
		t.Fatal(err)
	}

	var called atomic.Int32
	fw, err := NewFileWatcher(cfgFile, func() {
		called.Add(1)
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := fw.Start(ctx); err != nil {
			t.Errorf("Start() error: %v", err)
		}
	}()

	// Allow watcher to initialize.
	time.Sleep(100 * time.Millisecond)

	// Rapid modifications within the debounce window.
	for i := range 5 {
		if err := os.WriteFile(cfgFile, []byte("change-"+string(rune('0'+i))), 0644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Wait for debounce interval + buffer.
	time.Sleep(debounceInterval + 500*time.Millisecond)

	if got := called.Load(); got != 1 {
		t.Errorf("callback called %d times after rapid changes, want 1", got)
	}

	fw.Stop()
}

func TestFileWatcher_SymlinkSwap(t *testing.T) {
	dir := t.TempDir()

	// Create two versions of the config file.
	v1Dir := filepath.Join(dir, "v1")
	v2Dir := filepath.Join(dir, "v2")
	if err := os.Mkdir(v1Dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(v2Dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1Dir, "config.yaml"), []byte("v1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v2Dir, "config.yaml"), []byte("v2"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a "..data" symlink pointing to v1, like K8s does.
	dataLink := filepath.Join(dir, "..data")
	if err := os.Symlink(v1Dir, dataLink); err != nil {
		t.Fatal(err)
	}

	// Create config.yaml symlink -> ..data/config.yaml
	cfgLink := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(filepath.Join("..data", "config.yaml"), cfgLink); err != nil {
		t.Fatal(err)
	}

	var called atomic.Int32
	fw, err := NewFileWatcher(cfgLink, func() {
		called.Add(1)
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := fw.Start(ctx); err != nil {
			t.Errorf("Start() error: %v", err)
		}
	}()

	// Allow watcher to initialize.
	time.Sleep(100 * time.Millisecond)

	// Simulate K8s ConfigMap swap: atomically replace ..data symlink.
	tmpLink := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink(v2Dir, tmpLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpLink, dataLink); err != nil {
		t.Fatal(err)
	}

	// Wait for debounce interval + buffer.
	time.Sleep(debounceInterval + 500*time.Millisecond)

	if got := called.Load(); got != 1 {
		t.Errorf("callback called %d times after symlink swap, want 1", got)
	}

	fw.Stop()
}

func TestFileWatcher_Stop(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(cfgFile, []byte("initial"), 0644); err != nil {
		t.Fatal(err)
	}

	var called atomic.Int32
	fw, err := NewFileWatcher(cfgFile, func() {
		called.Add(1)
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := fw.Start(ctx); err != nil {
			t.Errorf("Start() error: %v", err)
		}
	}()

	// Allow watcher to initialize.
	time.Sleep(100 * time.Millisecond)

	// Stop should cause Start to return.
	fw.Stop()

	select {
	case <-done:
		// OK
	case <-time.After(3 * time.Second):
		t.Error("Start() did not return after Stop()")
	}

	// Double Stop should not panic.
	fw.Stop()

	// Modifications after stop should not trigger callback.
	if err := os.WriteFile(cfgFile, []byte("after-stop"), 0644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(debounceInterval + 500*time.Millisecond)

	if got := called.Load(); got != 0 {
		t.Errorf("callback called %d times after Stop(), want 0", got)
	}
}
