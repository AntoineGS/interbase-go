package interbase

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"unicode/utf8"
)

const (
	blobPreviewHeaderSize = 22
	maxBlobPreviewPrefix  = (64 << 20) - 1
)

var blobPreviewMagic = [8]byte{'\x00', 'I', 'B', 'B', 'L', 'O', 'B', 'P'}

type blobReadLimitContextKey struct{}

// BlobPreview is a bounded database/sql BLOB value. Prefix contains no more
// than the requested number of bytes; TotalLength is reported by InterBase's
// BLOB-info API, and Truncated reports whether the prefix omitted value bytes.
// Text and UTF8 preserve the driver's normal BLOB subtype and charset policy.
type BlobPreview struct {
	Prefix      []byte
	TotalLength int64
	Truncated   bool
	Text        bool
	UTF8        bool
}

// WithBlobReadLimit asks database/sql query paths to materialize at most
// maxPrefixBytes plus one BLOB byte. The extra byte is used only to determine
// whether the retained prefix was cut. Direct-cursor and other APIs are
// unaffected. A negative or excessive limit is rejected when the query runs.
func WithBlobReadLimit(ctx context.Context, maxPrefixBytes int) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, blobReadLimitContextKey{}, maxPrefixBytes)
}

func blobReadLimitFromContext(ctx context.Context) (int, bool, error) {
	if ctx == nil {
		return 0, false, nil
	}
	limit, ok := ctx.Value(blobReadLimitContextKey{}).(int)
	if !ok {
		return 0, false, nil
	}
	if limit < 0 || limit > maxBlobPreviewPrefix || limit > math.MaxInt32-1 {
		return 0, true, errors.New("interbase: BLOB prefix limit is outside the supported range")
	}
	return limit, true, nil
}

func encodeBlobPreview(preview BlobPreview) []byte {
	if preview.TotalLength < 0 || uint64(len(preview.Prefix)) > math.MaxUint32 {
		return nil
	}
	encoded := make([]byte, blobPreviewHeaderSize+len(preview.Prefix))
	copy(encoded[:8], blobPreviewMagic[:])
	encoded[8] = 1 // version
	if preview.Truncated {
		encoded[9] |= 1
	}
	if preview.Text {
		encoded[9] |= 2
	}
	if preview.UTF8 {
		encoded[9] |= 4
	}
	binary.LittleEndian.PutUint64(encoded[10:18], uint64(preview.TotalLength))
	binary.LittleEndian.PutUint32(encoded[18:22], uint32(len(preview.Prefix)))
	copy(encoded[blobPreviewHeaderSize:], preview.Prefix)
	return encoded
}

// DecodeBlobPreview recognizes values returned by database/sql queries with
// WithBlobReadLimit. It copies Prefix so the result does not alias the scanned
// driver value.
func DecodeBlobPreview(value any) (BlobPreview, bool) {
	encoded, ok := value.([]byte)
	if !ok || len(encoded) < blobPreviewHeaderSize || string(encoded[:8]) != string(blobPreviewMagic[:]) || encoded[8] != 1 {
		return BlobPreview{}, false
	}
	flags := encoded[9]
	if flags&^byte(7) != 0 {
		return BlobPreview{}, false
	}
	length := binary.LittleEndian.Uint32(encoded[18:22])
	if uint64(length) != uint64(len(encoded)-blobPreviewHeaderSize) {
		return BlobPreview{}, false
	}
	totalLength := binary.LittleEndian.Uint64(encoded[10:18])
	if totalLength > math.MaxInt64 {
		return BlobPreview{}, false
	}
	return BlobPreview{
		Prefix:      append([]byte(nil), encoded[blobPreviewHeaderSize:]...),
		TotalLength: int64(totalLength),
		Truncated:   flags&1 != 0,
		Text:        flags&2 != 0,
		UTF8:        flags&4 != 0,
	}, true
}

func safeBlobPreviewPrefix(prefix []byte, text, isUTF8 bool) []byte {
	if !text || !isUTF8 || utf8.Valid(prefix) {
		return prefix
	}
	for offset := 0; offset < len(prefix); {
		if !utf8.FullRune(prefix[offset:]) {
			return prefix[:offset]
		}
		_, size := utf8.DecodeRune(prefix[offset:])
		if size == 1 && prefix[offset] >= utf8.RuneSelf {
			return prefix[:offset]
		}
		offset += size
	}
	return prefix
}
