package service

import "testing"

func TestPathIsBrewManaged(t *testing.T) {
	t.Setenv("HOMEBREW_PREFIX", "/opt/homebrew")
	cases := []struct {
		path string
		want bool
	}{
		{"/opt/homebrew/Cellar/claude-pool/0.2.0/bin/claude-pool", true},
		{"/opt/homebrew/opt/claude-pool/bin/claude-pool", true},
		{"/opt/homebrew/bin/claude-pool", true},
		{"/usr/local/Cellar/claude-pool/0.2.0/bin/claude-pool", true},
		{"/Users/me/Code/claude-pool/claude-pool", false},
		{"/Users/me/go/bin/claude-pool", false},
		{"/opt/homebrew/Cellar/something-else/1.0/bin/x", false},
	}
	for _, c := range cases {
		if got := pathIsBrewManaged(c.path); got != c.want {
			t.Errorf("pathIsBrewManaged(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestBrewPrefixesHonorsEnv(t *testing.T) {
	t.Setenv("HOMEBREW_PREFIX", "/custom/brew")
	got := brewPrefixes()
	if len(got) != 1 || got[0] != "/custom/brew" {
		t.Fatalf("brewPrefixes() = %v, want [/custom/brew]", got)
	}
}
