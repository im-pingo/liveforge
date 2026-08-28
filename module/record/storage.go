package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/internal/localfs"
	"golang.org/x/sys/unix"
)

var (
	ErrInvalidRecordingID   = errors.New("invalid recording id")
	ErrRecordingNotFound    = errors.New("recording not found")
	ErrRecordingNotReady    = errors.New("recording is not ready")
	ErrRecordingNoMedia     = errors.New("recording contains no media frames")
	ErrRecordingCodecConfig = errors.New("recording codec configuration is incomplete")
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

type sidecarWriteObject interface {
	io.Writer
	Complete() error
	Fail() error
}

type sidecarMediaFile interface {
	CreateSidecar(string, os.FileMode) (sidecarWriteObject, error)
	WriteSidecarAtomic(string, []byte, os.FileMode) error
}

const metadataSuffix = ".liveforge.json"

type LocalStorage struct {
	root string
	fs   *localfs.Root
}

func newStorageForConfig(cfg config.RecordConfig) (*LocalStorage, string, error) {
	pattern := cfg.Path
	if pattern == "" {
		ext := "flv"
		switch strings.ToLower(strings.TrimSpace(cfg.Format)) {
		case "", "mp4", "fmp4":
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
	boundary, err := localfs.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("record storage root: %w", mapStorageError(err))
	}
	storage := &LocalStorage{root: boundary.Path(), fs: boundary}
	if err := storage.recoverPartials(context.Background()); err != nil {
		_ = boundary.Close()
		return nil, err
	}
	return storage, nil
}

func (s *LocalStorage) Root() string { return s.root }

func (s *LocalStorage) Close() error { return s.fs.Close() }

func (s *LocalStorage) Create(ctx context.Context, id string, info RecordingInfo) (WriteObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cleanID, err := cleanRecordingID(id)
	if err != nil {
		return nil, err
	}
	if _, err := s.fs.Stat(cleanID); err == nil {
		return nil, fmt.Errorf("create recording: file already exists")
	} else if !errors.Is(err, localfs.ErrNotFound) {
		return nil, fmt.Errorf("create recording: %w", mapStorageError(err))
	}
	pending, err := s.fs.CreatePending(cleanID+".partial", 0644)
	if err != nil {
		return nil, fmt.Errorf("create recording: %w", mapStorageError(err))
	}
	info.ID = cleanID
	info.State = RecordingActive
	if info.StartedAt.IsZero() {
		info.StartedAt = time.Now().UTC()
	}
	return &localWriteObject{
		storage:   s,
		pending:   pending,
		file:      pending.File,
		finalID:   cleanID,
		finalBase: filepath.Base(filepath.FromSlash(cleanID)),
		info:      info,
	}, nil
}

func (s *LocalStorage) List(ctx context.Context) ([]RecordingInfo, error) {
	entries, err := s.fs.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list recordings: %w", mapStorageError(err))
	}
	items := make([]RecordingInfo, 0, len(entries))
	for _, entry := range entries {
		if strings.HasSuffix(entry.RelPath, metadataSuffix) || strings.HasSuffix(entry.RelPath, ".partial") {
			continue
		}
		if isTSPlaybackArtifact(entry.RelPath) {
			continue
		}
		item := s.readMetadata(entry.RelPath)
		item.ID = entry.RelPath
		item.Size = entry.Size
		if item.StartedAt.IsZero() {
			item.StartedAt = entry.ModTime.UTC()
		}
		if item.State == "" {
			item.State = RecordingCompleted
			if strings.HasSuffix(entry.RelPath, ".failed") {
				item.State = RecordingFailed
			}
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].StartedAt.Equal(items[j].StartedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].StartedAt.After(items[j].StartedAt)
	})
	return items, nil
}

func isTSPlaybackArtifact(id string) bool {
	base := filepath.Base(filepath.FromSlash(id))
	if isOwnedTSPlaybackArtifact(base, "") {
		return true
	}
	if marker := strings.LastIndex(base, ".ts.segment_"); marker > 0 {
		return isOwnedTSPlaybackArtifact(base, base[:marker+len(".ts")])
	}
	if marker := strings.LastIndex(base, ".ts.m3u8"); marker > 0 {
		return isOwnedTSPlaybackArtifact(base, base[:marker+len(".ts")])
	}
	return false
}

func isOwnedTSPlaybackArtifact(base, ownerBase string) bool {
	playlistBase := "index.m3u8"
	if ownerBase != "" {
		playlistBase = ownerBase + ".m3u8"
	}
	if isTSArtifactVariant(base, playlistBase) {
		return true
	}
	prefix := ownerBase + "segment_"
	if ownerBase != "" {
		prefix = ownerBase + ".segment_"
	}
	if !strings.HasPrefix(base, prefix) {
		return false
	}
	remainder := strings.TrimPrefix(base, prefix)
	dot := strings.IndexByte(remainder, '.')
	if dot <= 0 {
		return false
	}
	for _, digit := range remainder[:dot] {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	artifactBase := prefix + remainder[:dot] + ".ts"
	return isTSArtifactVariant(base, artifactBase)
}

func isTSArtifactVariant(base, artifactBase string) bool {
	if base == artifactBase {
		return true
	}
	if !strings.HasPrefix(base, artifactBase) {
		return false
	}
	suffix := strings.TrimPrefix(base, artifactBase)
	switch suffix {
	case ".partial", ".failed":
		return true
	}
	if !strings.HasPrefix(suffix, ".orphan-") || !strings.HasSuffix(suffix, ".failed") {
		return false
	}
	orphan := strings.TrimSuffix(strings.TrimPrefix(suffix, ".orphan-"), ".failed")
	return validOrphanSuffix(orphan)
}

func tsSidecarOwnerBase(recordingBase string) (string, bool) {
	owner := recordingBase
	if strings.HasSuffix(owner, ".failed") {
		owner = strings.TrimSuffix(owner, ".failed")
		if marker := strings.LastIndex(owner, ".orphan-"); marker > 0 && validOrphanSuffix(owner[marker+len(".orphan-"):]) {
			owner = owner[:marker]
		}
	}
	return owner, strings.EqualFold(filepath.Ext(owner), ".ts")
}

func validOrphanSuffix(value string) bool {
	stamp, attempt, ok := strings.Cut(value, "-")
	if !ok || stamp == "" || attempt == "" || strings.Contains(attempt, "-") {
		return false
	}
	for _, part := range []string{stamp, attempt} {
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return false
			}
		}
	}
	return true
}

func (s *LocalStorage) Stat(ctx context.Context, id string) (RecordingInfo, error) {
	if err := ctx.Err(); err != nil {
		return RecordingInfo{}, err
	}
	cleanID, err := cleanRecordingID(id)
	if err != nil {
		return RecordingInfo{}, err
	}
	fileInfo, err := s.fs.Stat(cleanID)
	if err != nil {
		return RecordingInfo{}, mapStorageError(err)
	}
	info := s.readMetadata(cleanID)
	info.ID = cleanID
	info.Size = fileInfo.Size()
	if info.StartedAt.IsZero() {
		info.StartedAt = fileInfo.ModTime().UTC()
	}
	if info.State == "" {
		info.State = RecordingCompleted
		if strings.HasSuffix(cleanID, ".failed") {
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
	cleanID, err := cleanRecordingID(id)
	if err != nil {
		return nil, RecordingInfo{}, err
	}
	file, _, err := s.fs.Open(cleanID)
	if err != nil {
		return nil, RecordingInfo{}, fmt.Errorf("open recording: %w", mapStorageError(err))
	}
	return file, info, nil
}

func (s *LocalStorage) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cleanID, err := cleanRecordingID(id)
	if err != nil {
		return err
	}
	dirRel := filepath.ToSlash(filepath.Dir(filepath.FromSlash(cleanID)))
	if dirRel == "." {
		dirRel = ""
	}
	base := filepath.Base(filepath.FromSlash(cleanID))
	dir, err := s.fs.OpenDir(dirRel, false)
	if err != nil {
		return mapStorageError(err)
	}
	defer dir.Close()
	fileInfo, err := dir.Stat(base)
	if err != nil {
		return mapStorageError(err)
	}
	if !fileInfo.Mode().IsRegular() || strings.HasSuffix(cleanID, ".partial") {
		return ErrRecordingNotReady
	}
	if ownerBase, hasTSSidecars := tsSidecarOwnerBase(base); hasTSSidecars {
		entries, err := dir.ListAll(ctx)
		if err != nil {
			return fmt.Errorf("list recording sidecars: %w", mapStorageError(err))
		}
		for _, entry := range entries {
			entryBase := filepath.Base(filepath.FromSlash(entry.RelPath))
			if !isOwnedTSPlaybackArtifact(entryBase, ownerBase) {
				continue
			}
			if !entry.Mode.IsRegular() {
				return fmt.Errorf("delete recording sidecar %q: non-regular entry", entryBase)
			}
			if err := dir.Remove(entryBase); err != nil && !errors.Is(err, localfs.ErrNotFound) {
				return fmt.Errorf("delete recording sidecar: %w", mapStorageError(err))
			}
		}
	}
	if err := dir.Remove(base + metadataSuffix); err != nil && !errors.Is(err, localfs.ErrNotFound) {
		return fmt.Errorf("delete recording metadata: %w", mapStorageError(err))
	}
	if err := dir.Remove(base); err != nil {
		return fmt.Errorf("delete recording: %w", mapStorageError(err))
	}
	return nil
}

func (s *LocalStorage) Health(ctx context.Context) StorageHealth {
	health := StorageHealth{Backend: "local", Root: s.root}
	if err := ctx.Err(); err != nil {
		health.Error = err.Error()
		return health
	}
	var stat unix.Statfs_t
	if err := s.fs.Fstatfs(&stat); err != nil {
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

func cleanRecordingID(id string) (string, error) {
	id = filepath.ToSlash(strings.TrimSpace(id))
	if id == "" || id == "." || strings.ContainsRune(id, '\x00') || strings.Contains(id, "\\") || filepath.IsAbs(id) {
		return "", ErrInvalidRecordingID
	}
	clean := filepath.Clean(filepath.FromSlash(id))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", ErrInvalidRecordingID
	}
	return filepath.ToSlash(clean), nil
}

func (s *LocalStorage) readMetadata(id string) RecordingInfo {
	data, err := s.fs.ReadFile(id + metadataSuffix)
	if err != nil {
		return RecordingInfo{}
	}
	var info RecordingInfo
	if json.Unmarshal(data, &info) != nil {
		return RecordingInfo{}
	}
	return info
}

func (s *LocalStorage) writeMetadata(id string, info RecordingInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return s.fs.WriteFileAtomic(id+metadataSuffix, data, 0644)
}

type localWriteObject struct {
	storage   *LocalStorage
	pending   *localfs.Pending
	file      *os.File
	finalID   string
	finalBase string
	info      RecordingInfo
	closed    bool
}

func (o *localWriteObject) Write(p []byte) (int, error) { return o.file.Write(p) }
func (o *localWriteObject) Seek(offset int64, whence int) (int64, error) {
	return o.file.Seek(offset, whence)
}
func (o *localWriteObject) Name() string { return o.pending.Name() }
func (o *localWriteObject) Sync() error  { return o.file.Sync() }

func (o *localWriteObject) CreateSidecar(base string, perm os.FileMode) (sidecarWriteObject, error) {
	if o.closed {
		return nil, ErrRecordingNotReady
	}
	pending, err := o.pending.CreateSiblingPending(base+".partial", perm)
	if err != nil {
		return nil, mapStorageError(err)
	}
	return &localSidecarWriteObject{pending: pending, file: pending.File, finalBase: base}, nil
}

func (o *localWriteObject) WriteSidecarAtomic(base string, data []byte, perm os.FileMode) error {
	if o.closed {
		return ErrRecordingNotReady
	}
	return mapStorageError(o.pending.WriteSiblingAtomic(base, data, perm))
}

type localSidecarWriteObject struct {
	pending   *localfs.Pending
	file      *os.File
	finalBase string
	closed    bool
	finalized bool
}

func (o *localSidecarWriteObject) Write(data []byte) (int, error) {
	if o.closed || o.finalized {
		return 0, os.ErrClosed
	}
	return o.file.Write(data)
}

func (o *localSidecarWriteObject) Complete() error {
	if o.finalized {
		return ErrRecordingNotReady
	}
	if err := o.file.Sync(); err != nil {
		return errors.Join(err, o.Fail())
	}
	if err := o.file.Close(); err != nil {
		o.closed = true
		return errors.Join(err, o.failClosed())
	}
	o.closed = true
	if err := o.pending.PublishAs(o.finalBase); err != nil {
		return errors.Join(err, o.failClosed())
	}
	o.finalized = true
	return o.pending.Close()
}

func (o *localSidecarWriteObject) Fail() error {
	if o.finalized {
		return nil
	}
	if !o.closed {
		closeErr := o.file.Close()
		o.closed = true
		return errors.Join(closeErr, o.failClosed())
	}
	return o.failClosed()
}

func (o *localSidecarWriteObject) failClosed() error {
	if o.finalized {
		return nil
	}
	o.finalized = true
	_, moveErr := o.pending.PreserveAs(failedNameCandidate(o.finalBase))
	return errors.Join(moveErr, o.pending.Close())
}

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
	if err := o.pending.PublishAs(o.finalBase); err != nil {
		return o.failClosed(update, err)
	}
	info := mergeRecordingInfo(o.info, update)
	info.ID = o.info.ID
	info.State = RecordingCompleted
	info.CompletedAt = time.Now().UTC()
	if stat, err := o.pending.StatSibling(o.finalBase); err == nil {
		info.Size = stat.Size()
	}
	if err := o.writeMetadataSibling(o.finalBase, info); err != nil {
		failedID, renameErr := o.pending.MoveSiblingToUnique(o.finalBase, failedNameCandidate(o.finalBase))
		if renameErr != nil {
			_ = o.pending.Close()
			return info, fmt.Errorf("write recording metadata: %w; preserve failed recording: %v", err, renameErr)
		}
		info.ID = failedID
		info.State = RecordingFailed
		info.Error = "write recording metadata: " + err.Error()
		if metadataErr := o.writeMetadataSibling(filepath.Base(filepath.FromSlash(failedID)), info); metadataErr != nil {
			_ = o.pending.Close()
			return info, fmt.Errorf("write recording metadata: %w; write failed metadata: %v", err, metadataErr)
		}
		_ = o.pending.Close()
		return info, fmt.Errorf("write recording metadata: %w", err)
	}
	_ = o.pending.Close()
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
	failedID, err := o.pending.PreserveAs(failedNameCandidate(o.finalBase))
	if err != nil {
		_ = o.pending.Close()
		return RecordingInfo{}, fmt.Errorf("preserve failed recording: %w (original error: %v)", err, cause)
	}
	info := mergeRecordingInfo(o.info, update)
	info.ID = failedID
	info.State = RecordingFailed
	info.CompletedAt = time.Now().UTC()
	if cause != nil {
		info.Error = cause.Error()
	}
	failedBase := filepath.Base(filepath.FromSlash(failedID))
	if stat, err := o.pending.StatSibling(failedBase); err == nil {
		info.Size = stat.Size()
	}
	if err := o.writeMetadataSibling(failedBase, info); err != nil {
		_ = o.pending.Close()
		return info, fmt.Errorf("write failed recording metadata: %w", err)
	}
	_ = o.pending.Close()
	return info, nil
}

func (o *localWriteObject) writeMetadataSibling(mediaBase string, info RecordingInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return o.pending.WriteSiblingAtomic(mediaBase+metadataSuffix, data, 0644)
}

func failedNameCandidate(finalBase string) func(int) string {
	stamp := time.Now().UnixNano()
	return func(attempt int) string {
		if attempt == 0 {
			return finalBase + ".failed"
		}
		return fmt.Sprintf("%s.orphan-%d-%d.failed", finalBase, stamp, attempt)
	}
}

func (s *LocalStorage) recoverPartials(ctx context.Context) error {
	entries, err := s.fs.List(ctx, "")
	if err != nil {
		return fmt.Errorf("recover recording partials: %w", mapStorageError(err))
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.RelPath, ".partial") || strings.HasSuffix(entry.RelPath, metadataSuffix+".partial") {
			continue
		}
		original := strings.TrimSuffix(filepath.Base(filepath.FromSlash(entry.RelPath)), ".partial")
		failedID, err := s.fs.MoveToUnique(entry.RelPath, failedNameCandidate(original))
		if err != nil {
			return fmt.Errorf("recover recording partial %q: %w", entry.RelPath, mapStorageError(err))
		}
		if isTSPlaybackArtifact(original) {
			continue
		}
		info := RecordingInfo{
			ID:          failedID,
			State:       RecordingFailed,
			Size:        entry.Size,
			StartedAt:   entry.ModTime.UTC(),
			CompletedAt: time.Now().UTC(),
			Error:       "recovered incomplete recording after process interruption",
		}
		if err := s.writeMetadata(failedID, info); err != nil {
			return fmt.Errorf("write recovered recording metadata: %w", mapStorageError(err))
		}
	}
	return nil
}

func mapStorageError(err error) error {
	switch {
	case errors.Is(err, localfs.ErrInvalidPath):
		return ErrInvalidRecordingID
	case errors.Is(err, localfs.ErrNotFound):
		return ErrRecordingNotFound
	default:
		return err
	}
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
