package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"
)

// Admin is the single administrator account.
type Admin struct {
	Username string `json:"username"`
	Salt     []byte `json:"salt"`
	Hash     []byte `json:"hash"`
	Iter     int    `json:"iter"`
}

// Link is an external link shown on the 扩展 page.
type Link struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Desc      string `json:"desc"`
	Mode      string `json:"mode"` // "embed" or "new"
	Pin       bool   `json:"pin"`
	Proxy     bool   `json:"proxy"`    // embed through the NAS reverse proxy
	Insecure  bool   `json:"insecure"` // skip TLS verification for the target
	ProxyPort int    `json:"proxyPort,omitempty"`
}

// Mount is an external drive reachable over WebDAV.
type Mount struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"` // "webdav"
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
	Insecure bool   `json:"insecure"`
}

// Download is an offline download task.
type Download struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Name      string    `json:"name"`
	AutoName  bool      `json:"autoName"`
	Dir       string    `json:"dir"`
	Status    string    `json:"status"` // queued running paused done error canceled
	Total     int64     `json:"total"`
	Done      int64     `json:"done"`
	Speed     int64     `json:"speed"`
	Error     string    `json:"error"`
	MountID   string    `json:"mountId,omitempty"`
	MountPath string    `json:"mountPath,omitempty"`
	Created   time.Time `json:"created"`
	Finished  time.Time `json:"finished"`
}

// TrashItem records a deleted file kept under .trash/<ID>.
type TrashItem struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	OrigPath  string    `json:"origPath"`
	Dir       bool      `json:"dir"`
	Size      int64     `json:"size"`
	DeletedAt time.Time `json:"deletedAt"`
	// Home is the files directory whose .trash holds the item. Items of other
	// directories stay in the list untouched and show again when their
	// directory is chosen again.
	Home string `json:"home,omitempty"`
}

// DavKey lets one WebDAV client into one folder.
type DavKey struct {
	ID       string    `json:"id"`
	Username string    `json:"username"`
	Hash     []byte    `json:"hash"` // SHA-256 of the generated password
	Dir      string    `json:"dir"`  // folder the key is confined to; "" is everything
	ReadOnly bool      `json:"readOnly"`
	Created  time.Time `json:"created"`
}

// Settings are the options changed from the web UI.
type Settings struct {
	// TrashDays deletes recycle bin items after this many days; 0 keeps them
	// and nil means the default.
	TrashDays *int `json:"trashDays,omitempty"`
	// FilesDir is the files directory chosen in the web UI. It overrides the
	// command line. Empty when a hook stores the choice elsewhere.
	FilesDir string `json:"filesDir,omitempty"`
}

const (
	defaultTrashDays = 30
	maxTrashDays     = 3650
)

// State is everything persisted to state.json.
type State struct {
	Admin     *Admin       `json:"admin"`
	Links     []*Link      `json:"links"`
	Mounts    []*Mount     `json:"mounts"`
	Downloads []*Download  `json:"downloads"`
	Trash     []*TrashItem `json:"trash"`
	Settings  *Settings    `json:"settings"`
	DavKeys   []*DavKey    `json:"davKeys"`
	Seq       int          `json:"seq"`
}

// Store guards State with one mutex and persists it atomically.
// Callers lock mu, mutate, call save, unlock.
type Store struct {
	mu   sync.Mutex
	path string
	State
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.State); err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	return s, nil
}

// save writes the state to disk. The caller must hold mu.
func (s *Store) save() error {
	b, err := json.MarshalIndent(&s.State, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// trashDays returns the trash retention in days. The caller must hold mu.
func (s *Store) trashDays() int {
	if s.Settings == nil || s.Settings.TrashDays == nil {
		return defaultTrashDays
	}
	return *s.Settings.TrashDays
}

// nextID returns a new unique id. The caller must hold mu.
func (s *Store) nextID(prefix string) string {
	s.Seq++
	return fmt.Sprintf("%s%d", prefix, s.Seq)
}
