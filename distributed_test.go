package interbase

import (
	"context"
	"errors"
	"testing"
)

func TestBeginDistributedRejectsEmptyParticipants(t *testing.T) {
	_, err := BeginDistributed(context.Background(), nil)
	if !errors.Is(err, ErrDistributedNoParticipants) {
		t.Fatalf("BeginDistributed(nil) error = %v, want ErrDistributedNoParticipants", err)
	}
}

func TestBeginDistributedRejectsNilParticipantAttachment(t *testing.T) {
	_, err := BeginDistributed(context.Background(), []Participant{{}})
	if !errors.Is(err, ErrDistributedParticipantNil) {
		t.Fatalf("BeginDistributed(nil attachment) error = %v, want ErrDistributedParticipantNil", err)
	}
}

func TestBeginDistributedRejectsDuplicateAttachment(t *testing.T) {
	attachment := &Attachment{conn: &conn{}}
	_, err := BeginDistributed(context.Background(), []Participant{
		{Attachment: attachment},
		{Attachment: attachment},
	})
	if !errors.Is(err, ErrDistributedParticipantDuplicate) {
		t.Fatalf("BeginDistributed(duplicate attachment) error = %v, want ErrDistributedParticipantDuplicate", err)
	}
}

func TestBeginDistributedRejectsAttachmentWithExistingLocalTransaction(t *testing.T) {
	attachment := &Attachment{
		conn: &conn{},
	}
	attachment.directTx = &Transaction{attachment: attachment}

	_, err := BeginDistributed(context.Background(), []Participant{{Attachment: attachment}})
	if !errors.Is(err, ErrDistributedParticipantBusy) {
		t.Fatalf("BeginDistributed(active local transaction) error = %v, want ErrDistributedParticipantBusy", err)
	}
}
