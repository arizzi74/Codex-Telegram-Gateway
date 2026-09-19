package main

import (
	"context"
	"fmt"
	"io"

	"github.com/iaia/telegramgw/internal/registry"
)

type gatewayAdminSetupStore interface {
	AdminCredentials(context.Context) ([]registry.AdminCredential, error)
	BootstrapAdmin(context.Context) (string, error)
}

func bootstrapGatewayAdmin(ctx context.Context, store gatewayAdminSetupStore, origin string, onlyIfNeeded bool, out io.Writer) error {
	if onlyIfNeeded {
		credentials, err := store.AdminCredentials(ctx)
		if err != nil {
			return err
		}
		if len(credentials) > 0 {
			fmt.Fprintf(out, "An administrator passkey is already registered. Sign in at %s/tgadmin/.\n", origin)
			return nil
		}
	}
	token, err := store.BootstrapAdmin(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Open %s/tgadmin/ and register a passkey with this one-time token (expires in 15 minutes):\n%s\n", origin, token)
	return nil
}
