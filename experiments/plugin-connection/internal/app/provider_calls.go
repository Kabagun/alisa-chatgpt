package app

import (
	"context"
	"encoding/json"

	"smart-home-plugin/internal/provider"
)

// Vendor adapters must respect context deadlines. Contain adapter panics here
// so net/http cannot log a panic value that contains provider-private data.
func authenticateProvider(ctx context.Context, p provider.Provider, dir string, c provider.Credentials) (err error) {
	defer func() {
		if recover() != nil {
			err = provider.Err("provider_unavailable")
		}
	}()
	return p.Authenticate(ctx, dir, c)
}
func executeProvider(ctx context.Context, p provider.Provider, dir string, op provider.Operation, params provider.Params) (data json.RawMessage, err error) {
	defer func() {
		if recover() != nil {
			data = nil
			err = provider.Err("provider_unavailable")
		}
	}()
	return p.Execute(ctx, dir, op, params)
}
