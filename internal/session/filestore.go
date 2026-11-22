package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultDataDir returns the base directory for session storage,
// following XDG Base Directory conventions.
// Uses $XDG_DATA_HOME if set, otherwise falls back to ~/.local/share.
func DefaultDataDir() string {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.Getenv("HOME")
		}
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "codecuttlectl", "sessions")
}

// DefaultMaxSessions is the default maximum number of sessions to retain.
// Intentionally high — users can lower it if they care about disk usage.
const DefaultMaxSessions = 100000

// FileStore implements Store using local JSON files.
// Each session is stored as a single JSON file named {id}.json.
// Writes are atomic: data is written to a .tmp file, then renamed.
type FileStore struct {
	dir string
}

// NewFileStore creates a FileStore rooted at the given directory.
// Creates the directory if it doesn't exist.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("creating session directory %s: %w", dir, err)
	}
	return &FileStore{dir: dir}, nil
}

// Dir returns the storage directory path.
func (fs *FileStore) Dir() string {
	return fs.dir
}

// Create initializes a new session file with the given metadata.
// Returns the generated session ID.
func (fs *FileStore) Create(meta SessionMeta) (string, error) {
	id, err := GenerateID()
	if err != nil {
		return "", err
	}

	meta.ID = id
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now().UTC()
	}
	meta.UpdatedAt = meta.CreatedAt

	state := &SessionState{
		Meta:     meta,
		Messages: []Message{},
		Todos:    nil,
		Inkwell:  []InkEntry{},
	}

	if err := fs.Save(id, state); err != nil {
		return "", err
	}

	return id, nil
}

// Save persists the full session state atomically.
// Writes to a temporary file first, then renames to prevent corruption.
func (fs *FileStore) Save(id string, state *SessionState) error {
	if err := validateSessionID(id); err != nil {
		return err
	}
	state.Meta.UpdatedAt = time.Now().UTC()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling session %s: %w", id, err)
	}

	target := fs.filePath(id)
	tmp := target + ".tmp"

	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("writing session temp file %s: %w", tmp, err)
	}

	if err := os.Rename(tmp, target); err != nil {
		// Clean up temp file on rename failure
		os.Remove(tmp)
		return fmt.Errorf("renaming session file %s: %w", target, err)
	}

	return nil
}

// Load retrieves the full session state by ID.
func (fs *FileStore) Load(id string) (*SessionState, error) {
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(fs.filePath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("session %s not found", id)
		}
		return nil, fmt.Errorf("reading session %s: %w", id, err)
	}

	var state SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing session %s: %w", id, err)
	}

	return &state, nil
}

// List returns recent session metadata, sorted by updated_at descending.
// Only parses the "meta" field for efficiency.
func (fs *FileStore) List(limit int) ([]SessionMeta, error) {
	entries, err := os.ReadDir(fs.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing sessions directory: %w", err)
	}

	var metas []SessionMeta

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(fs.dir, name))
		if err != nil {
			continue // Skip unreadable files
		}

		// Partial decode: only parse the meta field for speed
		var partial struct {
			Meta SessionMeta `json:"meta"`
		}
		if err := json.Unmarshal(data, &partial); err != nil {
			continue // Skip malformed files
		}

		metas = append(metas, partial.Meta)
	}

	// Sort by updated_at descending (most recent first)
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].UpdatedAt.After(metas[j].UpdatedAt)
	})

	if limit > 0 && len(metas) > limit {
		metas = metas[:limit]
	}

	return metas, nil
}

// Delete removes a session and any archived epochs from storage.
func (fs *FileStore) Delete(id string) error {
	if err := validateSessionID(id); err != nil {
		return err
	}
	path := fs.filePath(id)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("session %s not found", id)
		}
		return fmt.Errorf("deleting session %s: %w", id, err)
	}
	// Cascade remove archived epochs for this session
	_ = os.RemoveAll(fs.epochDir(id))
	return nil
}

// Prune removes sessions older than maxAge (based on updated_at) and their archived epochs.
// Returns the number of sessions deleted.
func (fs *FileStore) Prune(maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(fs.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("listing sessions for prune: %w", err)
	}

	cutoff := time.Now().UTC().Add(-maxAge)
	deleted := 0

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(fs.dir, name))
		if err != nil {
			continue
		}

		var partial struct {
			Meta SessionMeta `json:"meta"`
		}
		if err := json.Unmarshal(data, &partial); err != nil {
			continue
		}

		if partial.Meta.UpdatedAt.Before(cutoff) {
			if err := os.Remove(filepath.Join(fs.dir, name)); err == nil {
				deleted++
				id := strings.TrimSuffix(name, ".json")
				_ = os.RemoveAll(fs.epochDir(id))
			}
		}
	}

	return deleted, nil
}

// SaveEpochArchive atomically persists an immutable closed epoch archive
// under epochs/{id}/epoch-NNN.json and returns its relative archive path.
func (fs *FileStore) SaveEpochArchive(id string, archive *EpochArchive) (string, error) {
	if err := validateSessionID(id); err != nil {
		return "", err
	}
	if archive == nil {
		return "", fmt.Errorf("epoch archive cannot be nil")
	}
	if archive.Epoch <= 0 {
		return "", fmt.Errorf("epoch number must be >= 1, got %d", archive.Epoch)
	}

	if archive.SessionID == "" {
		archive.SessionID = id
	}
	if archive.ClosedAt.IsZero() {
		archive.ClosedAt = time.Now().UTC()
	}
	if archive.Checkpoint.CreatedAt.IsZero() {
		archive.Checkpoint.CreatedAt = archive.ClosedAt
	}

	dir := fs.epochDir(id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("creating epoch directory for %s: %w", id, err)
	}

	filename := fmt.Sprintf("epoch-%03d.json", archive.Epoch)
	targetPath := filepath.Join(dir, filename)
	tmpPath := targetPath + ".tmp"
	relPath := filepath.ToSlash(filepath.Join("epochs", id, filename))
	if archive.Checkpoint.ArchiveFile == "" {
		archive.Checkpoint.ArchiveFile = relPath
	}

	data, err := json.MarshalIndent(archive, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling epoch archive: %w", err)
	}

	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return "", fmt.Errorf("writing epoch archive temp file: %w", err)
	}

	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("renaming epoch archive file: %w", err)
	}

	return relPath, nil
}

// LoadEpochArchive retrieves a specific closed epoch archive by session ID and epoch number.
func (fs *FileStore) LoadEpochArchive(id string, epoch int) (*EpochArchive, error) {
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	if epoch <= 0 {
		return nil, fmt.Errorf("epoch number must be >= 1, got %d", epoch)
	}

	filename := fmt.Sprintf("epoch-%03d.json", epoch)
	path := filepath.Join(fs.epochDir(id), filename)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("epoch %d archive for session %s not found", epoch, id)
		}
		return nil, fmt.Errorf("reading epoch archive %s: %w", path, err)
	}

	var archive EpochArchive
	if err := json.Unmarshal(data, &archive); err != nil {
		return nil, fmt.Errorf("unmarshaling epoch archive %s: %w", path, err)
	}

	return &archive, nil
}

// ListEpochArchives returns the sorted epoch numbers archived for a session.
func (fs *FileStore) ListEpochArchives(id string) ([]int, error) {
	if err := validateSessionID(id); err != nil {
		return nil, err
	}

	dir := fs.epochDir(id)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing epoch archives for %s: %w", id, err)
	}

	var epochs []int
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "epoch-") || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		var epochNum int
		if _, err := fmt.Sscanf(name, "epoch-%d.json", &epochNum); err == nil && epochNum > 0 {
			epochs = append(epochs, epochNum)
		}
	}

	sort.Ints(epochs)
	return epochs, nil
}

func (fs *FileStore) filePath(id string) string {
	return filepath.Join(fs.dir, id+".json")
}

func (fs *FileStore) epochDir(id string) string {
	return filepath.Join(fs.dir, "epochs", id)
}

func validateSessionID(id string) error {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return fmt.Errorf("session ID cannot be empty")
	}
	if strings.ContainsAny(trimmed, "/\\.") || trimmed == ".." {
		return fmt.Errorf("invalid session ID %q", id)
	}
	return nil
}
