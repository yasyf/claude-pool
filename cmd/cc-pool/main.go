// Command cc-pool is the single binary behind both `cc-pool` and its
// `clp` symlink.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/yasyf/cc-pool/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	root := cli.NewRootCmd()
	// Present the invoked name (clp or cc-pool) in help/usage.
	if base := filepath.Base(os.Args[0]); base == "clp" {
		root.Use = "clp"
	}

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
