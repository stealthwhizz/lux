//go:build windows

package cli

import "context"

func (a *App) server(ctx context.Context, args []string) error {
	return exitError{ExitError, "`lux server` is not supported on Windows yet; run luxd as a service or connect to a remote server with `lux login --url`"}
}
