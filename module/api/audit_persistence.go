package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/im-pingo/liveforge/config"
)

type auditPersistence struct {
	root     *os.Root
	lock     *os.File
	file     *os.File
	name     string
	size     int64
	maxBytes int64
}

// OpenAuditStore opens optional synchronous NDJSON persistence and restores
// retained history without re-emitting events or incrementing process counters.
func OpenAuditStore(cfg config.AuditConfig) (*AuditStore, error) {
	if err := config.ValidateAuditConfig(cfg); err != nil {
		return nil, err
	}
	store := NewAuditStore(cfg.MaxEntries)
	if cfg.Path == "" {
		return store, nil
	}
	if err := auditPersistenceSupported(); err != nil {
		return nil, err
	}
	path, err := config.ResolveUserPath(cfg.Path)
	if err != nil {
		return nil, err
	}
	if mkdirErr := os.MkdirAll(filepath.Dir(path), 0700); mkdirErr != nil {
		return nil, fmt.Errorf("create audit directory: %w", mkdirErr)
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("open audit directory: %w", err)
	}
	p := &auditPersistence{root: root, name: filepath.Base(path), maxBytes: cfg.MaxBytes}
	if p.maxBytes == 0 {
		p.maxBytes = config.DefaultAuditMaxBytes
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = p.close()
		}
	}()
	p.lock, err = p.openFile(p.name+".lock", os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, fmt.Errorf("open audit lock: %w", err)
	}
	if info, statErr := p.lock.Stat(); statErr != nil || info.Size() != 0 {
		return nil, fmt.Errorf("audit lock must be an empty regular file")
	}
	if lockErr := lockAuditFile(p.lock); lockErr != nil {
		return nil, fmt.Errorf("lock audit storage: %w", lockErr)
	}
	rotated, err := p.openFile(p.name+".1", os.O_RDWR)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("open rotated audit file: %w", err)
	}
	if rotated != nil {
		_, replayErr := p.replay(rotated, store)
		closeErr := rotated.Close()
		if restoreErr := errors.Join(replayErr, closeErr); restoreErr != nil {
			return nil, fmt.Errorf("restore rotated audit file: %w", restoreErr)
		}
	}
	p.file, err = p.openFile(p.name, os.O_RDWR|os.O_CREATE|os.O_APPEND)
	if err != nil {
		return nil, fmt.Errorf("open audit file: %w", err)
	}
	p.size, err = p.replay(p.file, store)
	if err != nil {
		return nil, fmt.Errorf("restore audit file: %w", err)
	}
	store.persistent = p
	store.status = AuditPersistenceStatus{Enabled: true, Healthy: true}
	cleanup = false
	return store, nil
}

func (p *auditPersistence) openFile(name string, flags int) (*os.File, error) {
	if info, err := p.root.Lstat(name); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("audit files must be regular and must not be symlinks")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := p.root.OpenFile(name, flags|auditOpenFlags(), 0600)
	if err != nil {
		return nil, err
	}
	if validationErr := p.validateFile(name, file); validationErr != nil {
		_ = file.Close()
		return nil, validationErr
	}
	return file, nil
}

func (p *auditPersistence) validateFile(name string, file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	linked, err := p.root.Lstat(name)
	if err != nil {
		return err
	}
	if !linked.Mode().IsRegular() || !info.Mode().IsRegular() || !os.SameFile(info, linked) {
		return errors.New("audit file changed or is not regular")
	}
	if info.Mode().Perm() != 0600 {
		return errors.New("audit files must have mode 0600")
	}
	return validateAuditLinks(file)
}

func (p *auditPersistence) replay(file *os.File, store *AuditStore) (int64, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() > p.maxBytes {
		return 0, errors.New("audit file exceeds api.audit.max_bytes")
	}
	reader := bufio.NewReaderSize(io.LimitReader(file, p.maxBytes+1), auditMaxLineBytes)
	var offset int64
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, io.EOF) {
			if len(line) != 0 {
				if truncateErr := file.Truncate(offset); truncateErr != nil {
					return 0, truncateErr
				}
				if syncErr := file.Sync(); syncErr != nil {
					return 0, syncErr
				}
			}
			return offset, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			return 0, errors.New("audit entry exceeds 64 KiB")
		}
		if err != nil {
			return 0, err
		}
		offset += int64(len(line))
		if offset > p.maxBytes {
			return 0, errors.New("audit file exceeds api.audit.max_bytes")
		}
		var entry AuditEntry
		if decodeErr := json.Unmarshal(line, &entry); decodeErr != nil {
			return 0, errors.New("audit file contains an invalid complete entry")
		}
		store.retain(sanitizeAuditEntry(entry))
	}
}

func (p *auditPersistence) append(entry AuditEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if len(data)+1 > auditMaxLineBytes {
		entry.Metadata = nil
		data, err = json.Marshal(entry)
		if err != nil {
			return err
		}
	}
	if len(data)+1 > auditMaxLineBytes {
		return errors.New("audit entry exceeds 64 KiB")
	}
	data = append(data, '\n')
	if validationErr := p.validateFile(p.name, p.file); validationErr != nil {
		return validationErr
	}
	info, err := p.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != p.size {
		return errors.New("audit file size changed outside its writer")
	}
	if int64(len(data)) > p.maxBytes-p.size {
		if rotateErr := p.rotate(); rotateErr != nil {
			return rotateErr
		}
	}
	n, err := p.file.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return errors.Join(err, p.file.Truncate(p.size))
	}
	p.size += int64(n)
	return nil
}

func (p *auditPersistence) rotate() error {
	if err := p.file.Sync(); err != nil {
		return err
	}
	rotated, err := p.openFile(p.name+".1", os.O_RDWR)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if rotated != nil {
		if closeErr := rotated.Close(); closeErr != nil {
			return closeErr
		}
		if removeErr := p.root.Remove(p.name + ".1"); removeErr != nil {
			return removeErr
		}
	}
	if closeErr := p.file.Close(); closeErr != nil {
		return closeErr
	}
	p.file = nil
	if renameErr := p.root.Rename(p.name, p.name+".1"); renameErr != nil {
		return renameErr
	}
	p.file, err = p.openFile(p.name, os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_APPEND)
	if err != nil {
		return err
	}
	p.size = 0
	return nil
}

func (p *auditPersistence) close() error {
	var result error
	if p.file != nil {
		result = errors.Join(p.file.Sync(), p.file.Close())
		p.file = nil
	}
	if p.lock != nil {
		result = errors.Join(result, p.lock.Close())
		p.lock = nil
	}
	if p.root != nil {
		result = errors.Join(result, p.root.Close())
		p.root = nil
	}
	return result
}

func auditPersistenceError(err error) string {
	var pathError *os.PathError
	var linkError *os.LinkError
	if errors.As(err, &pathError) {
		err = pathError.Err
	} else if errors.As(err, &linkError) {
		err = linkError.Err
	}
	return sanitizeAuditText(err.Error(), 512)
}
