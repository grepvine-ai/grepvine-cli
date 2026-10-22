package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// uploadZip uploads the files a zip archive holds, each under the path it has
// in the archive, without extracting the archive to disk.
//
// The central directory names every entry with its size before any content
// is read, so the upload knows its total at once and needs no counting pass.
// Entries are published in archive order, which is the order their data lies
// in the file, and the reader pool inflates the entries of a chunk side by
// side. One open archive costs a quarter of the processor time of the tree it
// holds, which pays an open, a stat and a close per file.
func (a *app) uploadZip(ctx context.Context, organization, index, source, prefix string) error {
	prefix, err := normalizeRemotePath(prefix, true)
	if err != nil {
		return fmt.Errorf("remote prefix: %w", err)
	}
	archive, info, err := openZip(source)
	if err != nil {
		return err
	}
	defer archive.Close()

	status := newProgress(a.stderr, a.isTTY)
	a.status.Store(status)
	defer a.status.Store(nil)
	status.start()
	defer status.finish()

	files := zipEntries(source, archive.File, prefix, info, func(message string) { status.printf("gv: warning: %s\n", message) })
	status.scanFinished(int64(len(files)))
	if len(files) == 0 {
		return errors.New("archive contains no files")
	}
	return a.uploadFiles(ctx, organization, index, status, true, func(ctx context.Context, emit func(uploadFile) error) error {
		for _, file := range files {
			if err := emit(file); err != nil {
				return err
			}
		}
		return nil
	}, encodeUploads)
}

// openZip opens a local archive and reads its central directory.
func openZip(source string) (*zip.ReadCloser, os.FileInfo, error) {
	archive, err := zip.OpenReader(source)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		// Names that would escape an extraction directory are refused one by
		// one by zipEntries, so they do not stop the rest of the archive.
		return nil, nil, fmt.Errorf("read %s: %w", source, err)
	}
	info, err := os.Stat(source)
	if err != nil {
		archive.Close()
		return nil, nil, fmt.Errorf("inspect source: %w", err)
	}
	return archive, info, nil
}

// zipEntries returns the files of an archive in archive order, each keyed by
// its path in the archive under prefix. Directories and the resource forks
// macOS adds under __MACOSX/ are left out without a word; links, encrypted
// entries, and names that are not a relative path are left out with a
// warning. A path that appears more than once is uploaded as its last copy,
// the one an extraction would leave behind.
func zipEntries(source string, entries []*zip.File, prefix string, info os.FileInfo, warn func(string)) []uploadFile {
	files := make([]uploadFile, 0, len(entries))
	position := make(map[string]int, len(entries))
	for _, entry := range entries {
		name := entry.Name
		if strings.HasSuffix(name, "/") || entry.Mode().IsDir() || name == "__MACOSX" || strings.HasPrefix(name, "__MACOSX/") {
			continue
		}
		file := uploadFile{localPath: source, entry: entry, size: int64(entry.UncompressedSize64), mtime: entry.Modified}
		if !entry.Mode().IsRegular() {
			warn(fmt.Sprintf("skipping %s: not a regular file", file.name()))
			continue
		}
		if entry.Flags&0x1 != 0 {
			warn(fmt.Sprintf("skipping %s: it is encrypted", file.name()))
			continue
		}
		key, err := normalizeRemotePath(name, false)
		if err == nil && prefix != "" {
			key, err = normalizeRemotePath(path.Join(prefix, key), false)
		}
		if err != nil {
			warn(fmt.Sprintf("skipping %s: path %s", file.name(), err))
			continue
		}
		file.key = key
		if file.mtime.IsZero() {
			file.mtime = info.ModTime()
		}
		if earlier, ok := position[key]; ok {
			warn(fmt.Sprintf("%s appears more than once; uploading its last copy", file.name()))
			files[earlier].entry = nil
		}
		position[key] = len(files)
		files = append(files, file)
	}
	kept := files[:0]
	for _, file := range files {
		if file.entry != nil {
			kept = append(kept, file)
		}
	}
	return kept
}

// readZipEntry decompresses one archive entry of at most maxBatchContent
// bytes. The size the central directory declares is the size the entry must
// inflate to, so an entry that inflates past it fails rather than growing a
// batch beyond its limit.
func readZipEntry(file uploadFile, index int, budget *chunkBudget) readResult {
	if result, ok := admitFile(file, index, budget); !ok {
		return result
	}
	content, err := readEntry(file.entry, file.size)
	if errors.Is(err, zip.ErrAlgorithm) {
		return readResult{file: file, skipped: "it is compressed with an unsupported method"}
	} else if err != nil {
		return readResult{file: file, err: fmt.Errorf("read %s: %w", file.name(), err)}
	}
	return readResult{file: file, content: content}
}

// readEntry reads an entry of size bytes through archive/zip, which checks
// the CRC-32 once the entry is read to its end.
func readEntry(entry *zip.File, size int64) ([]byte, error) {
	reader, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	content := make([]byte, size)
	return content, readExactly(reader, content)
}

// readExactly fills content from reader and fails unless the reader ends
// right after it.
func readExactly(reader io.Reader, content []byte) error {
	if _, err := io.ReadFull(reader, content); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	var probe [1]byte
	if _, err := io.ReadFull(reader, probe[:]); !errors.Is(err, io.EOF) {
		if err == nil {
			err = zip.ErrFormat
		}
		return err
	}
	return nil
}
