package service

import "testing"

func TestPathIsBrewManaged(t *testing.T) {
	t.Setenv("HOMEBREW_PREFIX", "/opt/homebrew")
	cases := []struct {
		path string
		want bool
	}{
		{"/opt/homebrew/Cellar/cc-pool/0.2.0/bin/cc-pool", true},
		{"/opt/homebrew/opt/cc-pool/bin/cc-pool", true},
		{"/opt/homebrew/bin/cc-pool", true},
		{"/usr/local/Cellar/cc-pool/0.2.0/bin/cc-pool", true},
		{"/Users/me/Code/cc-pool/cc-pool", false},
		{"/Users/me/go/bin/cc-pool", false},
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
