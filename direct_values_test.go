package interbase

import (
	"context"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestDirectValueAPISurface(t *testing.T) {
	var _ interface {
		OpenBlob(context.Context, BlobRef) (io.ReadCloser, error)
		CreateBlob(context.Context, BlobOptions, io.Reader) (BlobRef, error)
	} = (*Transaction)(nil)

	array := Array{
		Bounds: []ArrayBound{{Lower: -2, Upper: 0}, {Lower: 4, Upper: 5}},
		Elements: []any{
			int64(1), int64(2), int64(3),
			int64(4), int64(5), int64(6),
		},
	}
	if len(array.Bounds) != 2 || len(array.Elements) != 6 {
		t.Fatalf("array = %#v, want two bounds and six elements", array)
	}
}

func TestConvertArgumentAcceptsDirectArrayAndCopiesInput(t *testing.T) {
	input := Array{
		Bounds:   []ArrayBound{{Lower: 1, Upper: 2}},
		Elements: []any{int64(7), []byte("before")},
	}

	argument, err := convertArgument(input)
	if err != nil {
		t.Fatalf("convertArgument(array) error = %v", err)
	}
	if argument.kind != argumentArray {
		t.Fatalf("argument kind = %d, want argumentArray", argument.kind)
	}
	if argument.array == nil {
		t.Fatal("converted array is nil")
	}

	input.Bounds[0].Lower = 2
	input.Elements[1].([]byte)[0] = 'a'
	if argument.array.Bounds[0].Lower != 1 {
		t.Fatalf("converted bounds = %#v, input mutation leaked", argument.array.Bounds)
	}
	if string(argument.array.Elements[1].([]byte)) != "before" {
		t.Fatalf("converted bytes = %q, input mutation leaked", argument.array.Elements[1])
	}
}

func TestValidateDirectArray(t *testing.T) {
	tests := []struct {
		name    string
		value   Array
		wantErr string
	}{
		{
			name:    "no dimensions",
			value:   Array{Elements: []any{int64(1)}},
			wantErr: "at least one dimension",
		},
		{
			name:    "lower greater than upper",
			value:   Array{Bounds: []ArrayBound{{Lower: 2, Upper: 1}}},
			wantErr: "lower bound",
		},
		{
			name:    "too few elements",
			value:   Array{Bounds: []ArrayBound{{Lower: 0, Upper: 1}}, Elements: []any{int64(1)}},
			wantErr: "element count",
		},
		{
			name:    "too many elements",
			value:   Array{Bounds: []ArrayBound{{Lower: 0, Upper: 0}}, Elements: []any{int64(1), int64(2)}},
			wantErr: "element count",
		},
		{
			name:    "SDK lower bound overflow",
			value:   Array{Bounds: []ArrayBound{{Lower: math.MinInt32, Upper: 0}}, Elements: make([]any, 1)},
			wantErr: "outside InterBase array bound range",
		},
		{
			name:    "SDK upper bound overflow",
			value:   Array{Bounds: []ArrayBound{{Lower: 0, Upper: math.MaxInt32}}, Elements: make([]any, 1)},
			wantErr: "outside InterBase array bound range",
		},
		{
			name:    "unsupported element",
			value:   Array{Bounds: []ArrayBound{{Lower: 0, Upper: 0}}, Elements: []any{struct{}{}}},
			wantErr: "element 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDirectArray(tt.value)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateDirectArray(%#v) error = %v, want substring %q", tt.value, err, tt.wantErr)
			}
		})
	}

	valid := Array{
		Bounds:   []ArrayBound{{Lower: -2, Upper: 0}, {Lower: 4, Upper: 5}},
		Elements: []any{int64(1), int64(2), int64(3), int64(4), int64(5), int64(6)},
	}
	if err := validateDirectArray(valid); err != nil {
		t.Fatalf("validateDirectArray(valid) error = %v", err)
	}
}

func TestSnapshotDirectValueCopiesArray(t *testing.T) {
	source := Array{
		Bounds:   []ArrayBound{{Lower: 1, Upper: 1}},
		Elements: []any{[]byte("before")},
	}

	got, err := snapshotDirectValue(source)
	if err != nil {
		t.Fatalf("snapshotDirectValue(array) error = %v", err)
	}
	copyValue, ok := got.(Array)
	if !ok {
		t.Fatalf("snapshot type = %T, want Array", got)
	}

	source.Bounds[0].Lower = 99
	source.Elements[0].([]byte)[0] = 'a'
	if !reflect.DeepEqual(copyValue, Array{
		Bounds:   []ArrayBound{{Lower: 1, Upper: 1}},
		Elements: []any{[]byte("before")},
	}) {
		t.Fatalf("snapshot = %#v, input mutation leaked", copyValue)
	}
}

func TestBlobRefRejectsZeroValueWithoutNativeCall(t *testing.T) {
	transaction := &Transaction{}
	_, err := transaction.OpenBlob(context.Background(), BlobRef{})
	if err == nil {
		t.Fatal("OpenBlob(zero BlobRef) returned nil error")
	}
	if errors.Is(err, errDirectAttachmentClosed) {
		t.Fatalf("OpenBlob(zero BlobRef) error = %v, want invalid reference", err)
	}
}

func TestDirectBlobRefArgumentRequiresOwningTransaction(t *testing.T) {
	attachment := &Attachment{}
	owner := &Transaction{attachment: attachment, generation: 1}
	other := &Transaction{attachment: attachment, generation: 2}
	ref := BlobRef{
		attachment: attachment,
		tx:         owner,
		generation: owner.generation,
		high:       1,
		low:        2,
	}

	converted, err := convertArgument(ref)
	if err != nil {
		t.Fatalf("convertArgument(BlobRef) error = %v", err)
	}
	if err := validateDirectArguments([]argument{converted}, owner); err != nil {
		t.Fatalf("validateDirectArguments(owner) error = %v", err)
	}
	if err := validateDirectArguments([]argument{converted}, other); !errors.Is(err, errDirectBlobRefInvalid) {
		t.Fatalf("validateDirectArguments(other) error = %v, want invalid reference", err)
	}
}
