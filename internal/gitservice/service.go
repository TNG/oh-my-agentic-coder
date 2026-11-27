// Package gitservice provides the opt-in, single-repository Git service.
package gitservice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/pathutil"
)

const (
	maxEntries       = 100000
	maxOutput        = 4 << 20
	maxOperations    = 4096
	operationTimeout = 30 * time.Second
)

type Options struct {
	Workdir string
	Name    string
	Email   string
	Audit   func(action, result, head string)
}

type Service struct {
	mu                     sync.Mutex
	root, runtime, session string
	name, email            string
	rootInfo, gitInfo      os.FileInfo
	lock                   *os.File
	denied                 []string
	deniedParents          []string
	audit                  func(string, string, string)
	operations             map[string]record
	closed, uncertain      bool
	ready                  bool
	ctx                    context.Context
	cancel                 context.CancelFunc
}

func New(opts Options) (_ *Service, err error) {
	if err := supportedPlatform(); err != nil {
		return nil, err
	}
	for _, s := range []string{opts.Name, opts.Email} {
		if s == "" || len(s) > 256 || strings.ContainsAny(s, "\x00\r\n<>") {
			return nil, fmt.Errorf("git service requires an explicit name and email without control characters or angle brackets")
		}
		for _, c := range s {
			if c < 32 || c == 127 {
				return nil, fmt.Errorf("invalid Git identity")
			}
		}
	}
	root, err := filepath.EvalSymlinks(opts.Workdir)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil || root == "/" {
		return nil, fmt.Errorf("git service requires an ordinary checkout")
	}
	s := &Service{root: root, name: opts.Name, email: opts.Email, audit: opts.Audit, operations: map[string]record{}}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	s.rootInfo, err = os.Lstat(root)
	if err != nil {
		return nil, err
	}
	s.gitInfo, err = os.Lstat(filepath.Join(root, ".git"))
	if err != nil || !s.gitInfo.IsDir() {
		return nil, fmt.Errorf("git service supports only a real .git directory, not linked worktrees or separate gitdirs")
	}
	if err = s.validateRepository(); err != nil {
		return nil, err
	}
	s.lock, err = lockRepository(filepath.Join(root, ".git"))
	if err != nil {
		return nil, err
	}
	s.runtime, err = os.MkdirTemp("/tmp", "omac-git-service-")
	if err != nil {
		return nil, err
	}
	for _, d := range []string{"hooks", "empty"} {
		if err = os.Mkdir(filepath.Join(s.runtime, d), 0700); err != nil {
			return nil, err
		}
	}
	if err = os.WriteFile(filepath.Join(s.runtime, "source-config"), nil, 0600); err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	s.session = hex.EncodeToString(nonce[:])
	if err = os.WriteFile(filepath.Join(s.runtime, "config"), []byte(workerConfig(opts.Name, opts.Email, true)), 0600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(s.ctx, operationTimeout)
	defer cancel()
	version, err := s.run(ctx, false, false, nil, "version")
	if err != nil {
		return nil, fmt.Errorf("Git worker unavailable: %w", err)
	}
	var major, minor int
	if _, e := fmt.Sscanf(string(version), "git version %d.%d", &major, &minor); e != nil || major != 2 || minor < 43 {
		return nil, fmt.Errorf("git service requires Git 2.43 or newer in the 2.x series")
	}
	config, err := s.run(ctx, false, false, nil, "config", "--file", "/omac/source-config", "--no-includes", "--null", "--list")
	if err != nil {
		return nil, err
	}
	filemode, err := supportedConfig(config)
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(s.runtime, "config"), []byte(workerConfig(opts.Name, opts.Email, filemode)), 0600); err != nil {
		return nil, err
	}
	if _, err = s.state(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) RuntimeDir() string { return s.runtime }
func (s *Service) Workdir() string    { return s.root }

func (s *Service) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.lock != nil {
		s.lock.Close()
	}
	if s.runtime != "" {
		os.RemoveAll(s.runtime)
	}
}

// ConfigureDenies is called before the facade admits its first operation.
func (s *Service) ConfigureDenies(paths []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready || s.closed {
		return fmt.Errorf("Git service policy is fixed for the session")
	}
	for _, p := range paths {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("Git service denies must be canonical absolute paths")
		}
		if within(filepath.Join(s.root, ".git"), p) || within(p, filepath.Join(s.root, ".git")) {
			return fmt.Errorf("git service cannot override a metadata deny: %s", p)
		}
	}
	parents, err := pathutil.ExistingParentsWithin(s.root, paths)
	if err != nil {
		return err
	}
	s.deniedParents = parents
	s.denied = append([]string(nil), paths...)
	s.ready = true
	return nil
}

func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}
