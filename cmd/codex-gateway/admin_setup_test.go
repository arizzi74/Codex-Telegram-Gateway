package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/registry"
)

type adminSetupStore struct {
	credentials    []registry.AdminCredential
	err            error
	bootstrapCalls int
}

func (s *adminSetupStore) AdminCredentials(context.Context) ([]registry.AdminCredential, error) {
	return s.credentials, s.err
}
func (s *adminSetupStore) BootstrapAdmin(context.Context) (string, error) {
	s.bootstrapCalls++
	return "one-time-admin-token", nil
}

func TestBootstrapAdminIfNeededPreservesExistingAdministrators(t *testing.T) {
	for _, existing := range []bool{false, true} {
		store := &adminSetupStore{}
		if existing {
			store.credentials = []registry.AdminCredential{{ID: []byte("passkey")}}
		}
		var out bytes.Buffer
		if err := bootstrapGatewayAdmin(context.Background(), store, "https://gateway.example.com", true, &out); err != nil {
			t.Fatal(err)
		}
		if existing {
			if store.bootstrapCalls != 0 || strings.Contains(out.String(), "one-time-admin-token") || !strings.Contains(out.String(), "already registered") {
				t.Fatal("existing administrator was bootstrapped again")
			}
		} else if store.bootstrapCalls != 1 || !strings.Contains(out.String(), "one-time-admin-token") || !strings.Contains(out.String(), "15 minutes") {
			t.Fatal("first administrator was not guided through bootstrap")
		}
	}
}

func TestBootstrapAdminIfNeededDoesNotIgnoreRegistryFailure(t *testing.T) {
	store := &adminSetupStore{err: errors.New("registry unavailable")}
	var out bytes.Buffer
	if err := bootstrapGatewayAdmin(context.Background(), store, "https://gateway.example.com", true, &out); err == nil || store.bootstrapCalls != 0 || out.Len() != 0 {
		t.Fatal("failed administrator lookup was ignored")
	}
}

func TestExplicitAdminBootstrapStillWorksForRecovery(t *testing.T) {
	store := &adminSetupStore{credentials: []registry.AdminCredential{{ID: []byte("passkey")}}}
	var out bytes.Buffer
	if err := bootstrapGatewayAdmin(context.Background(), store, "https://gateway.example.com", false, &out); err != nil || store.bootstrapCalls != 1 {
		t.Fatal("explicit administrator recovery no longer works", err)
	}
}
