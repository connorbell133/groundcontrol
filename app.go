package main

import (
	"path/filepath"
	"sync"
)

// app owns all of the runner's mutable state — config, sessions, jobs, event
// subscribers, the folder browser, the journal, and the worktree base. main()
// constructs exactly one; tests construct their own (with a temp dataDir and
// wtBase) so instances stay fully isolated from each other.
type app struct {
	// configMu guards cfg. The PUT /config handler in api.go mutates the config
	// and calls applyAndPersistConfig while holding this lock; readers (auth
	// middleware, GET /config) take it briefly to snapshot.
	configMu   sync.Mutex
	cfg        Config
	configPath string

	sessionsMu sync.Mutex
	sessions   map[string]*liveSession
	// names reserved by launches still between duplicate-check and insertion
	pendingNames map[string]bool

	lostMu       sync.Mutex
	lostCache    []LostSession
	lostComputed bool

	jobsMu      sync.Mutex
	jobs        map[string]*liveJob
	jobQueue    []string
	jobsRunning int
	jobDefaults struct{ concurrency, timeoutMs int }

	eventsMu       sync.Mutex
	webhooks       []WebhookConfig
	eventListeners []eventListener
	nextListenerID int

	browserMu  sync.RWMutex
	browserCfg struct {
		roots      []string
		showHidden bool
	}

	// journal storage root; journalPath()/legacyJournalPath() derive from it
	dataDir         string
	journalMu       sync.Mutex
	journalMigrated bool // guarded by journalMu; migration runs at most once per process

	// base directory for runner-managed worktrees
	wtBase string
}

// newApp builds an instance with the production defaults: dataDir under the
// process cwd, wtBase under the user's home. Both are plain fields so a test
// can point a fresh instance at temp directories before serving anything.
func newApp(configPath string, cfg Config) *app {
	return &app{
		cfg:          cfg,
		configPath:   configPath,
		sessions:     map[string]*liveSession{},
		pendingNames: map[string]bool{},
		jobs:         map[string]*liveJob{},
		jobDefaults:  struct{ concurrency, timeoutMs int }{concurrency: 2, timeoutMs: 15 * 60 * 1000},
		dataDir:      filepath.Join(mustCwd(), "data"),
		wtBase:       filepath.Join(mustHome(), ".groundcontrol", "worktrees"),
	}
}
