package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunRejectsMissingConfigurationWithoutLeakingSecrets(t *testing.T) {
	t.Setenv("INTERBASE_DATABASE", "")
	t.Setenv("INTERBASE_USER", "alice")
	t.Setenv("INTERBASE_PASSWORD", "private-test-secret")
	var output bytes.Buffer
	err := run(context.Background(), &output)
	if err == nil {
		t.Fatal("missing database must fail before connection")
	}
	if strings.Contains(err.Error()+output.String(), "private-test-secret") {
		t.Fatal("probe leaked the password")
	}
}
