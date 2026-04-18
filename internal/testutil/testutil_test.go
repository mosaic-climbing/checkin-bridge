package testutil

import (
	"testing"
)

func TestFakeUniFiStarts(t *testing.T) {
	f := NewFakeUniFi()
	defer f.Close()
	if f.BaseURL() == "" {
		t.Error("expected non-empty base URL")
	}
}

func TestFakeRedpointStarts(t *testing.T) {
	f := NewFakeRedpoint()
	defer f.Close()
	if f.GraphQLURL() == "" {
		t.Error("expected non-empty GraphQL URL")
	}
}
