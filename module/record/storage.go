package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/im-pingo/liveforge/config"
)

var (
	ErrInvalidRecordingID = errors.New("invalid recording id")
	ErrRecordingNotFound  = errors.New("recording not found")
	ErrRecordingNotReady  = errors.New("recording is not ready")
)

type RecordingState string

const (
	RecordingActive    RecordingState = "active"
	RecordingCompleted RecordingState = "completed"
	RecordingFailed    RecordingState = "failed"
)

// RecordingInfo is the stable management representation of one recording.
// ID is a slash-separated path relative to the configured local storage root.
type RecordingInfo struct {
	ID          string         `json:"id"`
	StreamKey   string         `json:"stream_key,omitempty"`
	Format      string         `json:"format,omitempty"`
	State       RecordingState `json:"state"`
	Size        int64          `json:"size_bytes"`
	StartedAt   time.Time      `json:"started_at,omitempty"`
	CompletedAt time.Time      `json:"completed_at,omitempty"`
	DurationSec float64        `json:"duration_sec,omitempty"`
	Error       string         `json:"error,omitempty"`
}

type StorageHealth struct {
	Backend        string `json:"backend"`
	Root           string `json:"root"`
	Healthy        bool   `json:"healthy"`
	LowSpace       bool   `json:"low_space"`
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	Error          string `json:"error,omitempty"`
}

// ReadSeekCloser is suitable for ranged HTTP downloads via http.ServeContent.
type ReadSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
}

// Storage isolates recording management from the local filesystem backend.
// A future object backend can stage a seekable object and implement this API.
type Storage interface {
	Create(context.Context, string, RecordingInfo) (WriteObject, error)
	List(context.Context) ([]RecordingInfo, error)
	Stat(context.Context, string) (RecordingInfo, error)
	Open(context.Context, string) (ReadSeekCloser, RecordingInfo, error)
	Delete(context.Context, string) error
	Health(context.Context) StorageHealth
}

// RecordingProvider exposes recording management without coupling callers to
// the local storage implementation.
type RecordingProvider interface {
	ListRecordings(context.Context) ([]RecordingInfo, error)
	Recording(context.Context, string) (RecordingInfo, error)
	OpenRecording(context.Context, string) (ReadSeekCloser, RecordingInfo, error)
	DeleteRecording(context.Context, string) error
	RecordingStatus(context.Context) RecordingStatusSnapshot
}

// WriteObject is private until Complete atomically publishes it.
type WriteObject interface {
	io.WriteSeeker
	Name() string
	Sync() error
	Complete(context.Context, RecordingInfo) (RecordingInfo, error)
	Fail(context.Context, error) (RecordingInfo, error)
}

const metadataSuffix = ".liveforge.json"

type LocalStorage struct {
	root string
}

func newStorageForConfig(cfg config.RecordConfig) (*LocalStorage, string, error) {
	pattern := cfg.Path
	if pattern == "" {
		ext := "flv"
		switch strings.ToLower(cfg.Format) {
		case "mp4", "fmp4":
			ext = "mp4"
		case "ts", "hls":
			ext = "ts"
		}
		pattern = filepath.Join(".", "recordings", "{stream_key}", "{date}_{time}."+ext)
	}
	placeholder := strings.IndexByte(pattern, '{')
	if placeholder < 0 {
		root := filepath.Dir(pattern)
		storage, err := NewLocalStorage(root)
		return storage, filepath.Base(pattern), err
	}
	prefix := pattern[:placeholder]
	root := filepath.Clean(prefix)
	if strings.HasSuffix(prefix, string(filepath.Separator)) {
		root = filepath.Clean(prefix)
	} else {
		root = filepath.Dir(prefix)
	}
	rel, err := filepath.Rel(root, filepath.Clean(pattern))
	if err != nil {
		return nil, "", err
	}
	storage, err := NewLocalStorage(root)
	if err != nil {
		return nil, "", err
	}
	return storage, filepath.ToSlash(rel), nil
}

func NewLocalStorage(root string) (*LocalStorage, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("record storage root: %w", ErrInvalidRecordingID)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("record storage root: %w", err)
	}
	if err := os.MkdirAll(abs, 0755); err != nil {
		return nil, fmt.Errorf("record storage root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("record storage root: %w", err)
	}
	return &LocalStorage{root: filepath.Clean(resolved)}, nil
}

func (s *LocalStorage) Root() string { return s.root }

func (s *LocalStorage) Create(ctx context.Context, id string, info RecordingInfo) (WriteObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	finalPath, cleanID, err := s.objectPath(id, false)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0755); err != nil {
		return nil, fmt.Errorf("create recording directory: %w", err)
	}
	if err := s.ensureContained(filepath.Dir(finalPath)); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(finalPath); err == nil {
		return nil, fmt.Errorf("create recording: file already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("create recording: %w", err)
	}
	partialPath := finalPath + ".partial"
	file, err := os.OpenFile(partialPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("create recording: %w", err)
	}
	info.ID = cleanID
	info.State = RecordingActive
	if info.StartedAt.IsZero() {
		info.StartedAt = time.Now().UTC()
	}
	return &localWriteObject{storage: s, file: file, finalPath: finalPath, partialPath: partialPath, info: info}, nil
}

func (s *LocalStorage) List(ctx context.Context) ([]RecordingInfo, error) {
	items := make([]RecordingInfo, 0)
	err := filepath.WalkDir(s.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || strings.HasSuffix(entry.Name(), metadataSuffix) || strings.HasSuffix(entry.Name(), ".partial") {
			return nil
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if !fileInfo.Mode().IsRegular() {
			return nil
		}
		id, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}
		item := s.readMetadata(path)
		item.ID = filepath.ToSlash(id)
		item.Size = fileInfo.Size()
		if item.StartedAt.IsZero() {
			item.StartedAt = fileInfo.ModTime().UTC()
		}
		if item.State == "" {
			item.State = RecordingCompleted
			if strings.HasSuffix(path, ".failed") {
				item.State = RecordingFailed
			}
		}
		items = append(items, item)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list recordings: %w", err)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].StartedAt.Equal(items[j].StartedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].StartedAt.After(items[j].StartedAt)
	})
	return items, nil
}

func (s *LocalStorage) Stat(ctx context.Context, id string) (RecordingInfo, error) {
	if err := ctx.Err(); err != nil {
		return RecordingInfo{}, err
	}
	path, cleanID, err := s.objectPath(id, true)
	if err != nil {
		return RecordingInfo{}, err
	}
	fileInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return RecordingInfo{}, ErrRecordingNotFound
	}
	if err != nil {
		return RecordingInfo{}, fmt.Errorf("stat recording: %w", err)
	}
	if !fileInfo.Mode().IsRegular() {
		return RecordingInfo{}, ErrRecordingNotFound
	}
	info := s.readMetadata(path)
	info.ID = cleanID
	info.Size = fileInfo.Size()
	if info.StartedAt.IsZero() {
		info.StartedAt = fileInfo.ModTime().UTC()
	}
	if info.State == "" {
		info.State = RecordingCompleted
		if strings.HasSuffix(path, ".failed") {
			info.State = RecordingFailed
		}
	}
	return info, nil
}

func (s *LocalStorage) Open(ctx context.Context, id string) (ReadSeekCloser, RecordingInfo, error) {
	info, err := s.Stat(ctx, id)
	if err != nil {
		return nil, RecordingInfo{}, err
	}
	if info.State == RecordingActive {
		return nil, RecordingInfo{}, ErrRecordingNotReady
	}
	path, _, err := s.objectPath(id, true)
	if err != nil {
		return nil, RecordingInfo{}, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, RecordingInfo{}, ErrRecordingNotFound
	}
	if err != nil {
		return nil, RecordingInfo{}, fmt.Errorf("open recording: %w", err)
	}
	return file, info, nil
}

func (s *LocalStorage) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, _, err := s.objectPath(id, true)
	if err != nil {
		return err
	}
	fileInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrRecordingNotFound
	}
	if err != nil {
		return fmt.Errorf("stat recording for deletion: %w", err)
	}
	if !fileInfo.Mode().IsRegular() || strings.HasSuffix(path, ".partial") {
		return ErrRecordingNotReady
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("delete recording: %w", err)
	}
	if err := os.Remove(path + metadataSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete recording metadata: %w", err)
	}
	return nil
}

func (s *LocalStorage) Health(ctx context.Context) StorageHealth {
	health := StorageHealth{Backend: "local", Root: s.root}
	if err := ctx.Err(); err != nil {
		health.Error = err.Error()
		return health
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(s.root, &stat); err != nil {
		health.Error = err.Error()
		return health
	}
	health.TotalBytes = stat.Blocks * uint64(stat.Bsize)
	health.AvailableBytes = stat.Bavail * uint64(stat.Bsize)
	health.Healthy = health.TotalBytes > 0
	const minimumFree = uint64(100 << 20)
	health.LowSpace = health.AvailableBytes < minimumFree || (health.TotalBytes > 0 && health.AvailableBytes*100/health.TotalBytes < 5)
	return health
}

func (s *LocalStorage) objectPath(id string, requireExisting bool) (string, string, error) {
	id = filepath.ToSlash(strings.TrimSpace(id))
	if id == "" || id == "." || strings.ContainsRune(id, '\x00') || strings.Contains(id, "\\") || filepath.IsAbs(id) {
		return "", "", ErrInvalidRecordingID
	}
	clean := filepath.Clean(filepath.FromSlash(id))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", ErrInvalidRecordingID
	}
	path := filepath.Join(s.root, clean)
	if err := s.ensureLexicallyContained(path); err != nil {
		return "", "", err
	}
	if requireExisting {
		if _, err := os.Lstat(path); err == nil {
			if err := s.ensureContained(path); err != nil {
				return "", "", err
			}
		}
	}
	return path, filepath.ToSlash(clean), nil
}

func (s *LocalStorage) ensureLexicallyContained(path string) error {
	rel, err := filepath.Rel(s.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return ErrInvalidRecordingID
	}
	return nil
}

func (s *LocalStorage) ensureContained(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve recording path: %w", err)
	}
	return s.ensureLexicallyContained(resolved)
}

func (s *LocalStorage) readMetadata(path string) RecordingInfo {
	data, err := os.ReadFile(path + metadataSuffix)
	if err != nil {
		return RecordingInfo{}
	}
	var info RecordingInfo
	if json.Unmarshal(data, &info) != nil {
		return RecordingInfo{}
	}
	return info
}

func (s *LocalStorage) writeMetadata(path string, info RecordingInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	temp := path + metadataSuffix + ".partial"
	if err := os.WriteFile(temp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(temp, path+metadataSuffix); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

type localWriteObject struct {
	storage     *LocalStorage
	file        *os.File
	finalPath   string
	partialPath string
	info        RecordingInfo
	closed      bool
}

func (o *localWriteObject) Write(p []byte) (int, error) { return o.file.Write(p) }
func (o *localWriteObject) Seek(offset int64, whence int) (int64, error) {
	return o.file.Seek(offset, whence)
}
func (o *localWriteObject) Name() string { return o.partialPath }
func (o *localWriteObject) Sync() error  { return o.file.Sync() }

func (o *localWriteObject) Complete(ctx context.Context, update RecordingInfo) (RecordingInfo, error) {
	if err := ctx.Err(); err != nil {
		return RecordingInfo{}, err
	}
	if o.closed {
		return RecordingInfo{}, ErrRecordingNotReady
	}
	if err := o.file.Sync(); err != nil {
		return o.failAfterClose(update, err)
	}
	if err := o.file.Close(); err != nil {
		return o.failAfterClose(update, err)
	}
	o.closed = true
	if err := os.Rename(o.partialPath, o.finalPath); err != nil {
		return o.failClosed(update, err)
	}
	info := mergeRecordingInfo(o.info, update)
	info.ID = o.info.ID
	info.State = RecordingCompleted
	info.CompletedAt = time.Now().UTC()
	if stat, err := os.Stat(o.finalPath); err == nil {
		info.Size = stat.Size()
	}
	if err := o.storage.writeMetadata(o.finalPath, info); err != nil {
		failedPath := o.finalPath + ".failed"
		if renameErr := os.Rename(o.finalPath, failedPath); renameErr != nil {
			return info, fmt.Errorf("write recording metadata: %w; preserve failed recording: %v", err, renameErr)
		}
		info.ID += ".failed"
		info.State = RecordingFailed
		info.Error = "write recording metadata: " + err.Error()
		if metadataErr := o.storage.writeMetadata(failedPath, info); metadataErr != nil {
			return info, fmt.Errorf("write recording metadata: %w; write failed metadata: %v", err, metadataErr)
		}
		return info, fmt.Errorf("write recording metadata: %w", err)
	}
	return info, nil
}

func (o *localWriteObject) Fail(ctx context.Context, cause error) (RecordingInfo, error) {
	if err := ctx.Err(); err != nil {
		return RecordingInfo{}, err
	}
	if !o.closed {
		_ = o.file.Sync()
		_ = o.file.Close()
		o.closed = true
	}
	return o.failClosed(RecordingInfo{}, cause)
}

func (o *localWriteObject) failAfterClose(update RecordingInfo, cause error) (RecordingInfo, error) {
	_ = o.file.Close()
	o.closed = true
	return o.failClosed(update, cause)
}

func (o *localWriteObject) failClosed(update RecordingInfo, cause error) (RecordingInfo, error) {
	failedPath := o.finalPath + ".failed"
	if err := os.Rename(o.partialPath, failedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return RecordingInfo{}, fmt.Errorf("preserve failed recording: %w (original error: %v)", err, cause)
	}
	info := mergeRecordingInfo(o.info, update)
	info.ID += ".failed"
	info.State = RecordingFailed
	info.CompletedAt = time.Now().UTC()
	if cause != nil {
		info.Error = cause.Error()
	}
	if stat, err := os.Stat(failedPath); err == nil {
		info.Size = stat.Size()
	}
	if err := o.storage.writeMetadata(failedPath, info); err != nil {
		return info, fmt.Errorf("write failed recording metadata: %w", err)
	}
	return info, nil
}

func mergeRecordingInfo(base, update RecordingInfo) RecordingInfo {
	if update.StreamKey != "" {
		base.StreamKey = update.StreamKey
	}
	if update.Format != "" {
		base.Format = update.Format
	}
	if update.DurationSec != 0 {
		base.DurationSec = update.DurationSec
	}
	if !update.StartedAt.IsZero() {
		base.StartedAt = update.StartedAt
	}
	if update.Error != "" {
		base.Error = update.Error
	}
	return base
}
