package cli

import (
	"testing"

	"github.com/yasyf/cc-pool/internal/pool"
)

func TestMountHolderCommandRegisteredAndHidden(t *testing.T) {
	root := NewRootCmd()
	for _, cmd := range root.Commands() {
		if cmd.Name() != "mount-holder" {
			continue
		}
		if !cmd.Hidden {
			t.Error("mount-holder command is not hidden")
		}
		flag := cmd.Flags().Lookup("socket")
		if flag == nil {
			t.Fatal("mount-holder has no --socket flag")
		}
		if flag.DefValue != pool.MountsSocketPath() {
			t.Errorf("--socket default = %q, want %q", flag.DefValue, pool.MountsSocketPath())
		}
		return
	}
	t.Fatal("mount-holder command not registered on the root command")
}
