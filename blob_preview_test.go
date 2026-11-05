package interbase

import (
	"context"
	"reflect"
	"testing"
)

func TestDatabaseSQLBlobPreviewEncodingPreservesMetadata(t *testing.T) {
	want := BlobPreview{
		Prefix:      []byte{0x00, 0x01, 0xFF},
		TotalLength: 1_048_593,
		Truncated:   true,
		Text:        true,
		UTF8:        true,
	}

	got, ok := DecodeBlobPreview(encodeBlobPreview(want))
	if !ok {
		t.Fatal("DecodeBlobPreview rejected the driver's preview value")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded preview = %+v, want %+v", got, want)
	}
}

func TestBlobReadLimitIsScopedToQueryContext(t *testing.T) {
	base := context.Background()
	bounded := WithBlobReadLimit(base, 17)
	limit, ok, err := blobReadLimitFromContext(bounded)
	if err != nil || !ok || limit != 17 {
		t.Fatalf("bounded context limit = (%d, %t, %v), want (17, true, nil)", limit, ok, err)
	}
	if limit, ok, err := blobReadLimitFromContext(base); err != nil || ok || limit != 0 {
		t.Fatalf("base context limit = (%d, %t, %v), want (0, false, nil)", limit, ok, err)
	}
	if _, _, err := blobReadLimitFromContext(WithBlobReadLimit(base, -1)); err == nil {
		t.Fatal("negative BLOB prefix limit was accepted")
	}
}

func TestSafeBlobPreviewPrefixDropsOnlyIncompleteUTF8Suffix(t *testing.T) {
	for _, test := range []struct {
		name string
		data []byte
		want string
	}{
		{name: "complete", data: []byte("a界"), want: "a界"},
		{name: "partial rune", data: []byte{'a', 0xe7, 0x95}, want: "a"},
		{name: "invalid suffix", data: []byte{'a', 0xff}, want: "a"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := safeBlobPreviewPrefix(test.data, true, true)
			if string(got) != test.want {
				t.Fatalf("safe prefix = %q, want %q", got, test.want)
			}
		})
	}
	if got := safeBlobPreviewPrefix([]byte{0xe7, 0x95}, true, false); string(got) != string([]byte{0xe7, 0x95}) {
		t.Fatalf("non-UTF8 source bytes were altered: % X", got)
	}
}
