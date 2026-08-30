// Package localfs provides a descriptor-relative boundary for local media
// storage. All paths are resolved beneath a pinned root directory without
// following symlinks below that root.
package localfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var (
	ErrInvalidPath          = errors.New("invalid local storage path")
	ErrNotFound             = errors.New("local storage object not found")
	ErrExists               = errors.New("local storage object already exists")
	ErrHardLinksUnsupported = errors.New("local storage requires hard-link support")
)

var (
	linkAt    = unix.Linkat
	writeFile = func(file *os.File, data []byte) (int, error) {
		return file.Write(data)
	}
	syncFile = func(file *os.File) error {
		return file.Sync()
	}
	closeFile = func(file *os.File) error {
		return file.Close()
	}
)

type Root struct {
	path string
	fd   int
	once sync.Once
}

// Dir pins one directory below a Root so later path replacement cannot
// redirect file operations outside the directory that was originally opened.
type Dir struct {
	fd       int
	rel      string
	rootPath string
	once     sync.Once
}

type Entry struct {
	RelPath string
	Size    int64
	Mode    os.FileMode
	ModTime time.Time
}

type Pending struct {
	File     *os.File
	dirFD    int
	dirRel   string
	base     string
	rootPath string
	once     sync.Once
}

func OpenRoot(path string) (*Root, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	resolved, err := resolveRootTarget(abs)
	if err != nil {
		return nil, err
	}
	fd, err := openAbsoluteDir(resolved, true)
	if err != nil {
		return nil, err
	}
	if err := probeHardLinks(fd); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return &Root{path: filepath.Clean(resolved), fd: fd}, nil
}

func (r *Root) Path() string { return r.path }

func (r *Root) Close() error {
	var err error
	r.once.Do(func() { err = unix.Close(r.fd) })
	return err
}

func (r *Root) OpenDir(rel string, create bool) (*Dir, error) {
	fd, clean, err := r.openDirFD(rel, create)
	if err != nil {
		return nil, err
	}
	return &Dir{fd: fd, rel: clean, rootPath: r.path}, nil
}

func (r *Root) CreatePending(rel string, perm os.FileMode) (*Pending, error) {
	dirFD, dirRel, base, clean, err := r.openParent(rel, true)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(dirFD, base, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(perm.Perm()))
	if err != nil {
		_ = unix.Close(dirFD)
		return nil, mapPathError(err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(r.path, filepath.FromSlash(clean)))
	if file == nil {
		_ = unix.Close(fd)
		_ = unix.Close(dirFD)
		return nil, fmt.Errorf("create pending file")
	}
	return &Pending{File: file, dirFD: dirFD, dirRel: dirRel, base: base, rootPath: r.path}, nil
}

func (r *Root) Open(rel string) (*os.File, os.FileInfo, error) {
	dirFD, _, base, clean, err := r.openParent(rel, false)
	if err != nil {
		return nil, nil, err
	}
	defer unix.Close(dirFD)
	fd, err := unix.Openat(dirFD, base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, mapPathError(err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(r.path, filepath.FromSlash(clean)))
	if file == nil {
		_ = unix.Close(fd)
		return nil, nil, fmt.Errorf("open local storage file")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, ErrNotFound
	}
	return file, info, nil
}

func (r *Root) Stat(rel string) (os.FileInfo, error) {
	file, info, err := r.Open(rel)
	if err != nil {
		return nil, err
	}
	_ = file.Close()
	return info, nil
}

func (r *Root) ReadFile(rel string) ([]byte, error) {
	file, _, err := r.Open(rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func (r *Root) Remove(rel string) error {
	dirFD, _, base, _, err := r.openParent(rel, false)
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)
	var stat unix.Stat_t
	if err := unix.Fstatat(dirFD, base, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return mapPathError(err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrInvalidPath
	}
	if err := unix.Unlinkat(dirFD, base, 0); err != nil {
		return mapPathError(err)
	}
	return nil
}

func (r *Root) WriteFileAtomic(rel string, data []byte, perm os.FileMode) error {
	dirFD, _, base, _, err := r.openParent(rel, true)
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)
	return writeSiblingAtomic(dirFD, base, data, perm)
}

// MoveToUnique moves a regular file to a collision-safe sibling name without
// replacing an existing file. candidate receives the zero-based attempt.
func (r *Root) MoveToUnique(rel string, candidate func(int) string) (string, error) {
	dirFD, dirRel, base, _, err := r.openParent(rel, false)
	if err != nil {
		return "", err
	}
	defer unix.Close(dirFD)
	var stat unix.Stat_t
	if err := unix.Fstatat(dirFD, base, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return "", mapPathError(err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", ErrInvalidPath
	}
	name, err := moveSiblingToUnique(dirFD, base, candidate)
	if err != nil {
		return "", err
	}
	return joinRel(dirRel, name), nil
}

func (r *Root) List(ctx context.Context, relDir string) ([]Entry, error) {
	fd, clean, err := r.openDirFD(relDir, false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	entries := make([]Entry, 0)
	if err := walkDir(ctx, fd, clean, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (d *Dir) Path() string {
	return filepath.Join(d.rootPath, filepath.FromSlash(d.rel))
}

func (d *Dir) Close() error {
	var err error
	d.once.Do(func() { err = unix.Close(d.fd) })
	return err
}

func (d *Dir) CreatePending(base string, perm os.FileMode) (*Pending, error) {
	if !validBase(base) {
		return nil, ErrInvalidPath
	}
	dirFD, err := unix.Openat(d.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, mapPathError(err)
	}
	fd, err := unix.Openat(dirFD, base, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(perm.Perm()))
	if err != nil {
		_ = unix.Close(dirFD)
		return nil, mapPathError(err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(d.Path(), base))
	if file == nil {
		_ = unix.Close(fd)
		_ = unix.Close(dirFD)
		return nil, fmt.Errorf("create pending file")
	}
	return &Pending{File: file, dirFD: dirFD, dirRel: d.rel, base: base, rootPath: d.rootPath}, nil
}

func (d *Dir) Open(base string) (*os.File, os.FileInfo, error) {
	if !validBase(base) {
		return nil, nil, ErrInvalidPath
	}
	fd, err := unix.Openat(d.fd, base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, mapPathError(err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(d.Path(), base))
	if file == nil {
		_ = unix.Close(fd)
		return nil, nil, fmt.Errorf("open local storage file")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, ErrNotFound
	}
	return file, info, nil
}

func (d *Dir) Stat(base string) (os.FileInfo, error) {
	file, info, err := d.Open(base)
	if err != nil {
		return nil, err
	}
	_ = file.Close()
	return info, nil
}

func (d *Dir) Remove(base string) error {
	if !validBase(base) {
		return ErrInvalidPath
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(d.fd, base, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return mapPathError(err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrInvalidPath
	}
	if err := unix.Unlinkat(d.fd, base, 0); err != nil {
		return mapPathError(err)
	}
	return nil
}

func (d *Dir) MoveToUnique(base string, candidate func(int) string) (string, error) {
	if !validBase(base) {
		return "", ErrInvalidPath
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(d.fd, base, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return "", mapPathError(err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", ErrInvalidPath
	}
	return moveSiblingToUnique(d.fd, base, candidate)
}

func (d *Dir) List(ctx context.Context) ([]Entry, error) {
	return d.list(ctx, false)
}

// ListAll reports every direct child without following symbolic links.
func (d *Dir) ListAll(ctx context.Context) ([]Entry, error) {
	return d.list(ctx, true)
}

func (d *Dir) list(ctx context.Context, includeNonRegular bool) ([]Entry, error) {
	dup, err := unix.Openat(d.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, mapPathError(err)
	}
	dir := os.NewFile(uintptr(dup), d.Path())
	if dir == nil {
		_ = unix.Close(dup)
		return nil, fmt.Errorf("open directory listing")
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return nil, err
	}
	result := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(d.fd, entry.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return nil, mapPathError(err)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFREG && !includeNonRegular {
			continue
		}
		result = append(result, Entry{
			RelPath: joinRel(d.rel, entry.Name()),
			Size:    stat.Size,
			Mode:    entryFileMode(os.FileMode(stat.Mode)),
			ModTime: statModTime(stat),
		})
	}
	return result, nil
}

func entryFileMode(mode os.FileMode) os.FileMode {
	switch mode & os.FileMode(unix.S_IFMT) {
	case os.FileMode(unix.S_IFREG):
		return mode
	case os.FileMode(unix.S_IFDIR):
		return mode | os.ModeDir
	case os.FileMode(unix.S_IFLNK):
		return mode | os.ModeSymlink
	default:
		return mode | os.ModeIrregular
	}
}

func (r *Root) Fstatfs(stat *unix.Statfs_t) error { return unix.Fstatfs(r.fd, stat) }

func (p *Pending) Name() string {
	return filepath.Join(p.rootPath, filepath.FromSlash(joinRel(p.dirRel, p.base)))
}

func (p *Pending) PublishAs(finalBase string) error {
	if !validBase(finalBase) {
		return ErrInvalidPath
	}
	if err := linkAt(p.dirFD, p.base, p.dirFD, finalBase, 0); err != nil {
		return mapLinkError(err)
	}
	if err := unix.Unlinkat(p.dirFD, p.base, 0); err != nil {
		return err
	}
	p.base = finalBase
	return nil
}

func (p *Pending) MoveSiblingToUnique(fromBase string, candidate func(int) string) (string, error) {
	if !validBase(fromBase) {
		return "", ErrInvalidPath
	}
	name, err := moveSiblingToUnique(p.dirFD, fromBase, candidate)
	if err != nil {
		return "", err
	}
	p.base = name
	return joinRel(p.dirRel, name), nil
}

func (p *Pending) PreserveAs(candidate func(int) string) (string, error) {
	return p.MoveSiblingToUnique(p.base, candidate)
}

func (p *Pending) StatSibling(base string) (os.FileInfo, error) {
	if !validBase(base) {
		return nil, ErrInvalidPath
	}
	fd, err := unix.Openat(p.dirFD, base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, mapPathError(err)
	}
	file := os.NewFile(uintptr(fd), base)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("stat sibling")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrInvalidPath
	}
	return info, nil
}

// CreateSiblingPending creates an exclusive pending file in the directory
// pinned by p. Later path replacement cannot redirect the new object.
func (p *Pending) CreateSiblingPending(base string, perm os.FileMode) (*Pending, error) {
	if !validBase(base) {
		return nil, ErrInvalidPath
	}
	dirFD, err := unix.Openat(p.dirFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, mapPathError(err)
	}
	fd, err := unix.Openat(dirFD, base, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(perm.Perm()))
	if err != nil {
		_ = unix.Close(dirFD)
		return nil, mapPathError(err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(p.rootPath, filepath.FromSlash(joinRel(p.dirRel, base))))
	if file == nil {
		_ = unix.Close(fd)
		_ = unix.Close(dirFD)
		return nil, fmt.Errorf("create sibling pending file")
	}
	return &Pending{File: file, dirFD: dirFD, dirRel: p.dirRel, base: base, rootPath: p.rootPath}, nil
}

func (p *Pending) WriteSiblingAtomic(base string, data []byte, perm os.FileMode) error {
	if !validBase(base) {
		return ErrInvalidPath
	}
	return writeSiblingAtomic(p.dirFD, base, data, perm)
}

func (p *Pending) RelPath() string { return joinRel(p.dirRel, p.base) }

func (p *Pending) Close() error {
	var err error
	p.once.Do(func() { err = unix.Close(p.dirFD) })
	return err
}

func (r *Root) openParent(rel string, create bool) (int, string, string, string, error) {
	parts, clean, err := cleanParts(rel)
	if err != nil || len(parts) == 0 {
		return -1, "", "", "", ErrInvalidPath
	}
	dirParts := parts[:len(parts)-1]
	dirFD, err := r.openParts(dirParts, create)
	if err != nil {
		return -1, "", "", "", err
	}
	return dirFD, strings.Join(dirParts, "/"), parts[len(parts)-1], clean, nil
}

func (r *Root) openDirFD(rel string, create bool) (int, string, error) {
	if rel == "" || rel == "." {
		fd, err := unix.Openat(r.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		return fd, "", err
	}
	parts, clean, err := cleanParts(rel)
	if err != nil {
		return -1, "", err
	}
	fd, err := r.openParts(parts, create)
	return fd, clean, err
}

func (r *Root) openParts(parts []string, create bool) (int, error) {
	current, err := unix.Openat(r.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range parts {
		next, openErr := unix.Openat(current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil && create && errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(current, part, 0755); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(current)
				return -1, mapPathError(mkdirErr)
			}
			next, openErr = unix.Openat(current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		_ = unix.Close(current)
		if openErr != nil {
			return -1, mapPathError(openErr)
		}
		current = next
	}
	return current, nil
}

func resolveRootTarget(abs string) (string, error) {
	existing := filepath.Clean(abs)
	missing := make([]string, 0)
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", ErrInvalidPath
		}
		missing = append([]string{filepath.Base(existing)}, missing...)
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", ErrInvalidPath
	}
	parts := append([]string{resolved}, missing...)
	return filepath.Join(parts...), nil
}

func openAbsoluteDir(abs string, create bool) (int, error) {
	if !filepath.IsAbs(abs) {
		return -1, ErrInvalidPath
	}
	current, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(abs), string(filepath.Separator)), string(filepath.Separator))
	for _, part := range parts {
		if part == "" {
			continue
		}
		next, openErr := unix.Openat(current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil && create && errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(current, part, 0755); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(current)
				return -1, mapPathError(mkdirErr)
			}
			next, openErr = unix.Openat(current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		_ = unix.Close(current)
		if openErr != nil {
			return -1, mapPathError(openErr)
		}
		current = next
	}
	return current, nil
}

func walkDir(ctx context.Context, fd int, rel string, out *[]Entry) error {
	dup, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(dup), rel)
	if dir == nil {
		_ = unix.Close(dup)
		return fmt.Errorf("open directory listing")
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		var stat unix.Stat_t
		if err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return mapPathError(err)
		}
		mode := stat.Mode & unix.S_IFMT
		childRel := joinRel(rel, name)
		switch mode {
		case unix.S_IFDIR:
			childFD, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				return mapPathError(err)
			}
			err = walkDir(ctx, childFD, childRel, out)
			_ = unix.Close(childFD)
			if err != nil {
				return err
			}
		case unix.S_IFREG:
			*out = append(*out, Entry{
				RelPath: childRel,
				Size:    stat.Size,
				Mode:    os.FileMode(stat.Mode),
				ModTime: statModTime(stat),
			})
		}
	}
	return nil
}

func cleanParts(rel string) ([]string, string, error) {
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" || rel == "." || strings.ContainsRune(rel, '\x00') || strings.Contains(rel, "\\") || filepath.IsAbs(rel) {
		return nil, "", ErrInvalidPath
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return nil, "", ErrInvalidPath
	}
	parts := strings.Split(clean, "/")
	for _, part := range parts {
		if !validBase(part) {
			return nil, "", ErrInvalidPath
		}
	}
	return parts, clean, nil
}

func validBase(base string) bool {
	return base != "" && base != "." && base != ".." && !strings.Contains(base, "/") && !strings.Contains(base, "\\") && !strings.ContainsRune(base, '\x00')
}

func moveSiblingToUnique(dirFD int, from string, candidate func(int) string) (string, error) {
	for attempt := 0; attempt < 10000; attempt++ {
		to := candidate(attempt)
		if !validBase(to) {
			return "", ErrInvalidPath
		}
		if err := linkAt(dirFD, from, dirFD, to, 0); err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return "", mapLinkError(err)
		}
		if err := unix.Unlinkat(dirFD, from, 0); err != nil {
			return "", err
		}
		return to, nil
	}
	return "", ErrExists
}

func writeSiblingAtomic(dirFD int, final string, data []byte, perm os.FileMode) error {
	if !validBase(final) {
		return ErrInvalidPath
	}
	temp := fmt.Sprintf(".%s.tmp-%d", final, time.Now().UnixNano())
	fd, err := unix.Openat(dirFD, temp, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(perm.Perm()))
	if err != nil {
		return mapPathError(err)
	}
	file := os.NewFile(uintptr(fd), temp)
	_, writeErr := writeFile(file, data)
	var syncErr error
	if writeErr == nil {
		syncErr = syncFile(file)
	}
	closeErr := closeFile(file)
	var operationErr error
	switch {
	case writeErr != nil:
		operationErr = writeErr
	case syncErr != nil:
		operationErr = syncErr
	case closeErr != nil:
		operationErr = closeErr
	}
	var linkErr error
	if operationErr == nil {
		linkErr = linkAt(dirFD, temp, dirFD, final, 0)
	}
	removeErr := unix.Unlinkat(dirFD, temp, 0)
	if operationErr != nil {
		return errors.Join(operationErr, removeErr)
	}
	if linkErr != nil {
		return errors.Join(mapLinkError(linkErr), removeErr)
	}
	return removeErr
}

func probeHardLinks(dirFD int) error {
	stamp := time.Now().UnixNano()
	for attempt := 0; attempt < 100; attempt++ {
		source := fmt.Sprintf(".liveforge-link-probe-%d-%d", stamp, attempt)
		target := source + ".link"
		fd, err := unix.Openat(dirFD, source, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return mapPathError(err)
		}
		if err := unix.Close(fd); err != nil {
			_ = unix.Unlinkat(dirFD, source, 0)
			return err
		}
		linkErr := linkAt(dirFD, source, dirFD, target, 0)
		targetRemoveErr := error(nil)
		if linkErr == nil {
			targetRemoveErr = unix.Unlinkat(dirFD, target, 0)
		}
		sourceRemoveErr := unix.Unlinkat(dirFD, source, 0)
		if linkErr != nil {
			return fmt.Errorf("local storage hard-link probe: %w", mapLinkError(linkErr))
		}
		if err := errors.Join(targetRemoveErr, sourceRemoveErr); err != nil {
			return fmt.Errorf("local storage hard-link probe cleanup: %w", err)
		}
		return nil
	}
	return fmt.Errorf("local storage hard-link probe: %w", ErrExists)
}

func mapLinkError(err error) error {
	switch {
	case errors.Is(err, unix.ENOENT), errors.Is(err, unix.EEXIST), errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return mapPathError(err)
	case errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.ENOSYS):
		return fmt.Errorf("%w: %w", ErrHardLinksUnsupported, err)
	default:
		return err
	}
}

func mapPathError(err error) error {
	switch {
	case errors.Is(err, unix.ENOENT):
		return ErrNotFound
	case errors.Is(err, unix.EEXIST):
		return ErrExists
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return ErrInvalidPath
	default:
		return err
	}
}

func joinRel(dir, base string) string {
	if dir == "" {
		return base
	}
	return dir + "/" + base
}

func statModTime(stat unix.Stat_t) time.Time {
	return time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec)
}
