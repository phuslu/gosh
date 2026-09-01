package gosh

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
)

type mockBackend struct {
	mu sync.Mutex

	execCalls   [][]string
	openPaths   []string
	statNames   []string
	readDirDirs []string
	accessPaths []string

	execErr error
}

func (m *mockBackend) Exec(_ context.Context, args []string) error {
	m.mu.Lock()
	m.execCalls = append(m.execCalls, slices.Clone(args))
	err := m.execErr
	m.mu.Unlock()
	return err
}

func (m *mockBackend) Open(_ context.Context, path string, _ int, _ os.FileMode) (io.ReadWriteCloser, error) {
	m.mu.Lock()
	m.openPaths = append(m.openPaths, path)
	m.mu.Unlock()
	return &mockReadWriteCloser{Reader: strings.NewReader("backend-input\n")}, nil
}

func (m *mockBackend) Stat(_ context.Context, name string, _ bool) (fs.FileInfo, error) {
	m.mu.Lock()
	m.statNames = append(m.statNames, name)
	m.mu.Unlock()
	return mockFileInfo{name: filepath.Base(name), size: 4, mode: 0o644}, nil
}

func (m *mockBackend) ReadDir(_ context.Context, path string) ([]fs.DirEntry, error) {
	m.mu.Lock()
	m.readDirDirs = append(m.readDirDirs, path)
	m.mu.Unlock()
	return []fs.DirEntry{mockDirEntry{name: "mock-file.txt"}}, nil
}

func (m *mockBackend) Access(_ context.Context, path string, _ interp.AccessMode) error {
	m.mu.Lock()
	m.accessPaths = append(m.accessPaths, path)
	m.mu.Unlock()
	return nil
}

func (m *mockBackend) snapshot() (exec [][]string, open, stat, dirs, access []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.execCalls), slices.Clone(m.openPaths), slices.Clone(m.statNames), slices.Clone(m.readDirDirs), slices.Clone(m.accessPaths)
}

type mockReadWriteCloser struct {
	io.Reader
	io.Writer
}

func (m *mockReadWriteCloser) Close() error { return nil }

type mockFileInfo struct {
	name string
	size int64
	mode fs.FileMode
}

func (m mockFileInfo) Name() string       { return m.name }
func (m mockFileInfo) Size() int64        { return m.size }
func (m mockFileInfo) Mode() fs.FileMode  { return m.mode }
func (m mockFileInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (m mockFileInfo) IsDir() bool        { return false }
func (m mockFileInfo) Sys() any           { return nil }

type mockDirEntry struct {
	name string
}

func (m mockDirEntry) Name() string               { return m.name }
func (m mockDirEntry) IsDir() bool                { return false }
func (m mockDirEntry) Type() fs.FileMode          { return 0 }
func (m mockDirEntry) Info() (fs.FileInfo, error) { return mockFileInfo{name: m.name}, nil }

func TestBackendExec(t *testing.T) {
	backend := &mockBackend{}
	if err := Run(Config{
		Args:    []string{"gosh", "-c", "some-command"},
		Stdout:  io.Discard,
		Stderr:  io.Discard,
		Env:     testEnv(t),
		Backend: backend,
	}); err != nil {
		t.Fatalf("Run with mock Exec backend failed: %v", err)
	}
	exec, _, _, _, _ := backend.snapshot()
	if want := [][]string{{"some-command"}}; !equalStringSlices(exec, want) {
		t.Fatalf("exec calls = %#v, want %#v", exec, want)
	}
}

func TestBackendFilesystemHandlers(t *testing.T) {
	backend := &mockBackend{}
	var stdout, stderr bytes.Buffer
	err := Run(Config{
		Args: []string{"gosh", "-c", `
			read line <input
			printf '%s\n' "$line"
			[ -f input ] && echo stat-ok
			[ -r input ] && echo access-ok
			for f in *; do echo "$f"; done
		`},
		Stdout:  &stdout,
		Stderr:  &stderr,
		Env:     testEnv(t),
		Dir:     t.TempDir(),
		Backend: backend,
	})
	if err != nil {
		t.Fatalf("Run with mock filesystem backend failed: %v\nstderr: %s", err, stderr.String())
	}
	want := "backend-input\nstat-ok\naccess-ok\nmock-file.txt\n"
	if got := stdout.String(); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	_, open, stat, dirs, access := backend.snapshot()
	if len(open) == 0 {
		t.Fatal("Open backend was not used for the input redirect")
	}
	if len(stat) == 0 {
		t.Fatal("Stat backend was not used for [ -f ]")
	}
	if len(access) == 0 {
		t.Fatal("Access backend was not used for [ -r ]")
	}
	if len(dirs) == 0 {
		t.Fatal("ReadDir backend was not used for glob expansion")
	}
}

func TestBackendExecErrorBecomesStatus(t *testing.T) {
	backend := &mockBackend{execErr: interp.ExitStatus(7)}
	err := Run(Config{
		Args:    []string{"gosh", "-c", "some-command"},
		Stdout:  io.Discard,
		Stderr:  io.Discard,
		Env:     testEnv(t),
		Backend: backend,
	})
	if ExitCode(err) != 7 {
		t.Fatalf("ExitCode = %d, want 7 (err: %v)", ExitCode(err), err)
	}
}

func equalStringSlices(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !slices.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
