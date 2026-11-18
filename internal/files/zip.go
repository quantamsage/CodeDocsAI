package files

import (
	"archive/zip"
	"errors"
	"io"
	"io/fs"
	"strings"
)

// ratioCheckThreshold is the amount of output an archive entry may produce
// before its compression ratio is checked. Tiny, highly repetitive files
// legitimately compress very well.
const ratioCheckThreshold = 256 << 10

// minEntryLimit is the lowest cap on the number of entries an archive may
// list, whatever MaxFiles is.
const minEntryLimit = 1000

// AddZip extracts a ZIP archive into the batch. The whole archive is
// rejected if any entry has an unsafe path, is a symbolic link, or if the
// archive exceeds the entry count, total size or compression ratio limits.
// Entries that are merely unsupported are skipped.
//
// If every file sits under one top-level directory (as when a project folder
// is zipped), that directory is removed so paths are relative to the project
// root. The rest of the directory structure is preserved.
func (b *Batch) AddZip(ra io.ReaderAt, size int64) error {
	zr, err := zip.NewReader(ra, size)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return reject(ErrBadArchive, "The ZIP archive could not be read. Make sure it is a valid, complete .zip file.")
	}

	maxEntries := max(b.limits.MaxFiles*10, minEntryLimit)
	if len(zr.File) > maxEntries {
		return reject(ErrTooManyFiles, "The ZIP archive lists more than %d entries.", maxEntries)
	}

	type entry struct {
		file *zip.File
		path string
	}
	entries := make([]entry, 0, len(zr.File))
	for _, f := range zr.File {
		if f.Mode()&fs.ModeSymlink != 0 {
			return reject(ErrUnsafePath, "The ZIP archive contains a symbolic link (%q), which is not allowed.", displayName(f.Name))
		}
		p, err := CleanPath(f.Name)
		if err != nil {
			if f.FileInfo().IsDir() && strings.Trim(strings.ReplaceAll(f.Name, `\`, "/"), "/.") == "" {
				continue // a root "./" directory entry
			}
			return &RejectedError{Message: "The ZIP archive contains an unsafe path: " + strings.TrimPrefix(err.Error(), ErrUnsafePath.Error()+" ") + ".", Err: ErrUnsafePath}
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if !f.Mode().IsRegular() {
			return reject(ErrUnsafePath, "The ZIP archive contains a special file (%q), which is not allowed.", displayName(f.Name))
		}
		entries = append(entries, entry{file: f, path: p})
	}

	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.path
	}
	root := commonRoot(paths)

	for _, e := range entries {
		p := strings.TrimPrefix(e.path, root)
		if err := b.addZipEntry(e.file, p); err != nil {
			return err
		}
	}
	return nil
}

func (b *Batch) addZipEntry(f *zip.File, p string) error {
	if f.Flags&0x1 != 0 {
		b.skip(p, ReasonEncrypted)
		return nil
	}
	if f.Method != zip.Store && f.Method != zip.Deflate {
		b.skip(p, ReasonCompression)
		return nil
	}
	// The declared size lets oversized entries be skipped without reading
	// them; it is not trusted beyond that (reads are limited regardless).
	declared := b.limits.MaxFileBytes + 1
	if f.UncompressedSize64 <= uint64(b.limits.MaxFileBytes) {
		declared = int64(f.UncompressedSize64)
	}

	rc, err := f.Open()
	if err != nil {
		return reject(ErrBadArchive, "The ZIP entry %q could not be read.", displayName(p))
	}
	defer rc.Close()

	reader := &ratioReader{
		r:          rc,
		compressed: int64(f.CompressedSize64),
		maxRatio:   int64(b.limits.MaxCompressionRatio),
		path:       p,
	}
	return b.Add(p, reader, declared)
}

// ratioReader fails once an entry has expanded beyond maxRatio times its
// compressed size (after a small threshold).
type ratioReader struct {
	r          io.Reader
	compressed int64
	maxRatio   int64
	read       int64
	path       string
}

func (r *ratioReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.read += int64(n)
	if r.read > ratioCheckThreshold && r.read > max(r.compressed, 1)*r.maxRatio {
		return n, reject(ErrArchiveBomb,
			"The ZIP entry %q expands more than %d times its compressed size and looks like a ZIP bomb.",
			displayName(r.path), r.maxRatio)
	}
	return n, err
}

// commonRoot returns "dir/" if every path (ignoring OS metadata folders such
// as __MACOSX) lies under the same top-level directory, else "".
func commonRoot(paths []string) string {
	root := ""
	found := false
	for _, p := range paths {
		if IgnoredDir(p) != "" && strings.EqualFold(strings.SplitN(p, "/", 2)[0], "__MACOSX") {
			continue
		}
		first, rest, ok := strings.Cut(p, "/")
		if !ok || rest == "" {
			return ""
		}
		if !found {
			root, found = first, true
		} else if first != root {
			return ""
		}
	}
	if !found {
		return ""
	}
	return root + "/"
}
