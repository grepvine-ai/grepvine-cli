package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"
)

const (
	// walkReaders bounds how many directories the walk lists at once.
	// Enumerating a wide tree costs one readdir of latency per directory and
	// almost no processor time, so listing siblings concurrently is what makes
	// a source of several million files enumerable in seconds.
	walkReaders = 16
	// walkReadAhead bounds how many listings the walk holds beyond the entry
	// its depth-first consumer is on. The bound is global rather than one
	// window per level, so a deep tree reads less far ahead instead of holding
	// a window for every level it has descended through.
	walkReadAhead = 4 * walkReaders
)

// dirListing is one directory read, held until the depth-first walk reaches it.
type dirListing struct {
	entries []os.DirEntry
	err     error
}

func readDirListing(directory string) dirListing {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return dirListing{err: fmt.Errorf("read %s: %w", directory, err)}
	}
	return dirListing{entries: entries}
}

// sourceWalker streams the regular files under a source in the order
// filepath.WalkDir would visit them, reading directories concurrently.
//
// It never stats a file. The kernel already reports whether a directory entry
// is a file, a directory, or a symbolic link, so enumeration needs nothing
// else; size and modification time come from the handle the uploader opens to
// read the content, which happens on the reader pool while earlier batches are
// already in flight. Statting during enumeration instead costs one syscall per
// file on one goroutine, which is minutes of dead time before the first upload
// on a source of a few million files.
type sourceWalker struct {
	ctx     context.Context
	prefix  string
	ignores *gitIgnoreMatcher
	emit    func(uploadFile) error
	seen    func(directoriesFound, directoriesDone, files int64)

	tasks       chan func()
	workers     sync.WaitGroup
	outstanding atomic.Int64
	found       atomic.Int64
	done        atomic.Int64
	files       atomic.Int64
}

// countSource lists source once and reports how many files an upload of it
// would publish, applying the same ignore rules the upload will.
//
// Counting first is what lets the upload report a real total and a real
// estimate. The alternative, letting the upload discover the source as it goes,
// holds the walk back to the rate of the uploader, so a source of a few million
// files would be described as still being enumerated an hour into uploading it.
// A pass that only lists directories runs at over a million files a second, and
// it warms the page cache for the pass that follows.
func countSource(ctx context.Context, source string, seen func(directoriesFound, directoriesDone, files int64)) (int64, error) {
	walker, err := walkTree(ctx, source, "", nil, seen)
	if err != nil {
		return 0, err
	}
	return walker.files.Load(), nil
}

// walkSource calls emit for every regular file under source, in the
// deterministic depth-first, lexically sorted order of filepath.WalkDir. It
// reports progress through seen, which may be nil.
func walkSource(ctx context.Context, source, prefix string, emit func(uploadFile) error, seen func(directoriesFound, directoriesDone, files int64)) error {
	_, err := walkTree(ctx, source, prefix, emit, seen)
	return err
}

// walkTree runs one pass over source. A nil emit counts the files without
// building a key for any of them, which is all the counting pass needs.
func walkTree(ctx context.Context, source, prefix string, emit func(uploadFile) error, seen func(directoriesFound, directoriesDone, files int64)) (*sourceWalker, error) {
	walker := &sourceWalker{
		ctx: ctx, prefix: prefix, ignores: newGitIgnoreMatcher(source), emit: emit, seen: seen,
		tasks: make(chan func(), walkReadAhead),
	}
	rootInfo, err := os.Lstat(source)
	if err != nil {
		return walker, fmt.Errorf("inspect source: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return walker, errors.New("source cannot be a symbolic link")
	}
	if !rootInfo.IsDir() {
		if !rootInfo.Mode().IsRegular() {
			return walker, nil
		}
		walker.files.Add(1)
		if emit == nil {
			return walker, nil
		}
		key, err := remoteKey(prefix, filepath.Base(source))
		if err != nil {
			return walker, fmt.Errorf("map %s: %w", source, err)
		}
		return walker, emit(uploadFile{localPath: source, key: key})
	}
	for range walkReaders {
		walker.workers.Go(func() {
			for task := range walker.tasks {
				task()
			}
		})
	}
	defer func() {
		close(walker.tasks)
		walker.workers.Wait()
	}()
	walker.found.Add(1)
	if err := walker.walkDirectory(source, "", readDirListing(source)); err != nil {
		return walker, fmt.Errorf("walk source: %w", err)
	}
	walker.report()
	return walker, nil
}

func (w *sourceWalker) report() {
	if w.seen != nil {
		w.seen(w.found.Load(), w.done.Load(), w.files.Load())
	}
}

// walkDirectory consumes one already-read listing. It applies the ignore rules
// of this directory before it looks at any child, so a pruned subtree is never
// listed, and it visits files and subdirectories in one lexical order so the
// stream matches filepath.WalkDir entry for entry.
func (w *sourceWalker) walkDirectory(directory, relative string, listing dirListing) error {
	if listing.err != nil {
		return listing.err
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	w.done.Add(1)
	if err := w.ignores.AddDirectory(directory); err != nil {
		return err
	}
	kept := listing.entries[:0]
	subdirectories := make([]string, 0, 8)
	// Files are counted as the listing is filtered rather than as they are
	// handed on, because the uploader holds the walk back to its own rate: a
	// count of what has been named would only ever restate what has already
	// been uploaded, while a count of what has been seen is the lower bound
	// the progress line projects a total from.
	files := 0
	for _, entry := range listing.entries {
		name := entry.Name()
		if name == ".git" {
			continue
		}
		child := relative + name
		if w.ignores.Match(child, entry.IsDir()) {
			continue
		}
		mode := entry.Type()
		if mode&os.ModeSymlink != 0 {
			continue
		}
		switch {
		case mode.IsDir():
			subdirectories = append(subdirectories, filepath.Join(directory, name))
		case mode.IsRegular():
			files++
		default:
			continue
		}
		kept = append(kept, entry)
	}
	w.found.Add(int64(len(subdirectories)))
	w.files.Add(int64(files))
	w.report()
	ahead := w.readAhead(subdirectories)
	descended := 0
	for _, entry := range kept {
		name := entry.Name()
		local := filepath.Join(directory, name)
		if entry.IsDir() {
			if err := w.walkDirectory(local, relative+name+"/", ahead.take(descended)); err != nil {
				return err
			}
			descended++
			w.report()
			continue
		}
		if w.emit == nil {
			continue
		}
		key, err := remoteKey(w.prefix, relative+name)
		if err != nil {
			return fmt.Errorf("map %s: %w", local, err)
		}
		if err := w.emit(uploadFile{localPath: local, key: key}); err != nil {
			return err
		}
	}
	return nil
}

func remoteKey(prefix, relative string) (string, error) {
	if prefix == "" {
		return normalizeRemotePath(relative, false)
	}
	return normalizeRemotePath(path.Join(prefix, relative), false)
}

// dirReadAhead lists directories before the depth-first walk reaches them and
// hands the listings back in the order the walk asks for, so reading ahead
// changes only when the syscalls happen and never what the walk produces.
type dirReadAhead struct {
	walk    *sourceWalker
	paths   []string
	results []chan dirListing
	started int
}

func (w *sourceWalker) readAhead(paths []string) *dirReadAhead {
	if len(paths) == 0 {
		return &dirReadAhead{walk: w}
	}
	ahead := &dirReadAhead{walk: w, paths: paths, results: make([]chan dirListing, len(paths))}
	ahead.fill()
	return ahead
}

func (r *dirReadAhead) fill() {
	for r.started < len(r.paths) && r.walk.outstanding.Load() < walkReadAhead {
		index := r.started
		r.started++
		r.walk.outstanding.Add(1)
		result := make(chan dirListing, 1)
		r.results[index] = result
		directory := r.paths[index]
		r.walk.tasks <- func() { result <- readDirListing(directory) }
	}
}

func (r *dirReadAhead) take(index int) dirListing {
	result := r.results[index]
	if result == nil {
		// The read ahead stopped short of this directory because the walk was
		// already holding its share of listings. Reading it here keeps the
		// walk moving without letting the pool run any further ahead.
		if index >= r.started {
			r.started = index + 1
		}
		return readDirListing(r.paths[index])
	}
	r.results[index] = nil
	listing := <-result
	r.walk.outstanding.Add(-1)
	r.fill()
	return listing
}
