package files

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Limits bounds an upload. Sizes are uncompressed bytes.
type Limits struct {
	MaxFileBytes        int64
	MaxFiles            int
	MaxTotalBytes       int64
	MaxCompressionRatio int
}

// Errors that reject a whole upload. They wrap ErrRejected so handlers can
// show their message to the user.
var (
	ErrRejected     = errors.New("upload rejected")
	ErrTooManyFiles = fmt.Errorf("%w: too many files", ErrRejected)
	ErrTooLarge     = fmt.Errorf("%w: too much data", ErrRejected)
	ErrArchiveBomb  = fmt.Errorf("%w: suspicious compression ratio", ErrRejected)
	ErrBadArchive   = fmt.Errorf("%w: unreadable archive", ErrRejected)
)

// RejectedError is an upload rejection with a user-facing message.
type RejectedError struct {
	Message string
	Err     error // one of the Err* values above, or ErrUnsafePath
}

func (e *RejectedError) Error() string { return e.Message }
func (e *RejectedError) Unwrap() []error {
	return []error{e.Err, ErrRejected}
}

func reject(kind error, format string, args ...any) error {
	return &RejectedError{Message: fmt.Sprintf(format, args...), Err: kind}
}

// Skipped is a file that was left out of an upload, with the reason.
type Skipped struct {
	Path   string
	Reason string
}

// Skip reasons.
const (
	ReasonIgnoredDir   = "dependency, build or VCS directory"
	ReasonSystemFile   = "operating system metadata file"
	ReasonSecret       = "may contain secrets"
	ReasonArchive      = "archives inside uploads are not extracted"
	ReasonExecutable   = "executable or compiled binary"
	ReasonUnsupported  = "unsupported file type"
	ReasonBinary       = "not a UTF-8 text file"
	ReasonTooLarge     = "larger than the per-file limit"
	ReasonDuplicate    = "duplicate path in this upload"
	ReasonEncrypted    = "encrypted archive entry"
	ReasonCompression  = "unsupported compression method"
	maxSkippedReported = 200
)

// Staged is a validated file whose content has been written to blob storage
// but not yet recorded in the database.
type Staged struct {
	Path        string
	Language    string
	Kind        string
	Size        int64
	SHA256      string
	StoragePath string
}

// Batch collects the files of one upload. Files are validated and written to
// blob storage one at a time, so memory use is bounded by the per-file limit.
// Call Abort if the batch is not committed.
type Batch struct {
	blobs     *BlobStore
	projectID int64
	limits    Limits

	staged       []Staged
	seen         map[string]bool
	skipped      []Skipped
	skippedTotal int
	totalBytes   int64
}

// NewBatch starts an upload for projectID.
func NewBatch(blobs *BlobStore, projectID int64, limits Limits) *Batch {
	return &Batch{blobs: blobs, projectID: projectID, limits: limits, seen: map[string]bool{}}
}

// Staged returns the accepted files.
func (b *Batch) Staged() []Staged { return b.staged }

// Skipped returns skipped files (at most the first 200) and the total count.
func (b *Batch) Skipped() ([]Skipped, int) { return b.skipped, b.skippedTotal }

func (b *Batch) skip(p, reason string) {
	b.skippedTotal++
	if len(b.skipped) < maxSkippedReported {
		b.skipped = append(b.skipped, Skipped{Path: displayName(p), Reason: reason})
	}
}

// Add validates one file and stages it. Problems with the individual file
// (unsupported type, binary content, too large, …) skip it; an unsafe path or
// exceeding the upload-wide limits rejects the whole upload with an error.
// declaredSize is the size claimed by the source (-1 if unknown); it lets
// oversized archive entries be skipped without reading them.
func (b *Batch) Add(rawPath string, r io.Reader, declaredSize int64) error {
	p, err := CleanPath(rawPath)
	if err != nil {
		return &RejectedError{Message: "The upload contains an unsafe file path: " + strings.TrimPrefix(err.Error(), ErrUnsafePath.Error()+" ") + ".", Err: ErrUnsafePath}
	}

	switch {
	case IgnoredDir(p) != "":
		b.skip(p, ReasonIgnoredDir)
		return nil
	case IsIgnoredFile(p):
		b.skip(p, ReasonSystemFile)
		return nil
	case IsSecretFile(p):
		b.skip(p, ReasonSecret)
		return nil
	case IsArchive(p):
		b.skip(p, ReasonArchive)
		return nil
	case IsExecutableName(p):
		b.skip(p, ReasonExecutable)
		return nil
	}
	class, ok := Classify(p)
	if !ok {
		b.skip(p, ReasonUnsupported)
		return nil
	}
	if b.seen[p] {
		b.skip(p, ReasonDuplicate)
		return nil
	}
	if declaredSize > b.limits.MaxFileBytes {
		b.skip(p, ReasonTooLarge)
		return nil
	}

	data, err := io.ReadAll(io.LimitReader(r, b.limits.MaxFileBytes+1))
	if err != nil {
		var rej *RejectedError
		if errors.As(err, &rej) {
			return err
		}
		return reject(ErrBadArchive, "The file %q could not be read: the upload may be corrupt or incomplete.", displayName(p))
	}
	if int64(len(data)) > b.limits.MaxFileBytes {
		b.skip(p, ReasonTooLarge)
		return nil
	}
	if IsExecutableContent(data) {
		b.skip(p, ReasonExecutable)
		return nil
	}
	if !IsText(data) {
		b.skip(p, ReasonBinary)
		return nil
	}

	if len(b.staged) >= b.limits.MaxFiles {
		return reject(ErrTooManyFiles, "The upload contains more than %d supported files.", b.limits.MaxFiles)
	}
	if b.totalBytes+int64(len(data)) > b.limits.MaxTotalBytes {
		return reject(ErrTooLarge, "The upload contains more than %s of files.", formatSize(b.limits.MaxTotalBytes))
	}

	storagePath, err := b.blobs.Write(b.projectID, data)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	b.seen[p] = true
	b.totalBytes += int64(len(data))
	b.staged = append(b.staged, Staged{
		Path:        p,
		Language:    class.Language,
		Kind:        class.Kind,
		Size:        int64(len(data)),
		SHA256:      hex.EncodeToString(sum[:]),
		StoragePath: storagePath,
	})
	return nil
}

// Abort removes the blobs of every staged file.
func (b *Batch) Abort() {
	for _, s := range b.staged {
		b.blobs.Remove(s.StoragePath) //nolint:errcheck // best effort cleanup
	}
	b.staged = nil
}

// formatSize renders a byte count with a binary unit for messages.
func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
