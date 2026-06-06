// Command claude-pool is the single binary behind both `claude-pool` and its
// `clp` symlink.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/yasyf/claude-pool/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	root := cli.NewRootCmd()
	// Present the invoked name (clp or claude-pool) in help/usage.
	if base := filepath.Base(os.Args[0]); base == "clp" {
		root.Use = "clp"
	}

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
