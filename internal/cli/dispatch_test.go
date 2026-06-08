package cli

import (
	"slices"
	"testing"
)

func TestInjectRun(t *testing.T) {
	cases := map[string]struct {
		in   []string
		want []string
	}{
		"bare ccp unchanged":            {nil, nil},
		"empty slice unchanged":         {[]string{}, []string{}},
		"resume flag forwards":          {[]string{"--resume"}, []string{"run", "--resume"}},
		"short -p with value forwards":  {[]string{"-p", "hi"}, []string{"run", "-p", "hi"}},
		"flag then positional forwards": {[]string{"--resume", "foo"}, []string{"run", "--resume", "foo"}},
		"status subcommand untouched":   {[]string{"status"}, []string{"status"}},
		"run not double-injected":       {[]string{"run", "--resume"}, []string{"run", "--resume"}},
		"help word untouched":           {[]string{"help"}, []string{"help"}},
		"--help is ccp's own":           {[]string{"--help"}, []string{"--help"}},
		"-h is ccp's own":               {[]string{"-h"}, []string{"-h"}},
		"--version is ccp's own":        {[]string{"--version"}, []string{"--version"}},
		"-v is ccp's own":               {[]string{"-v"}, []string{"-v"}},
		"typo positional preserved":     {[]string{"stauts"}, []string{"stauts"}},
		"bare positional not forwarded": {[]string{"summarize this"}, []string{"summarize this"}},
		"empty first arg not forwarded": {[]string{""}, []string{""}},
		"double dash forwards":          {[]string{"--", "--resume"}, []string{"run", "--", "--resume"}},
		"combined short flags forward":  {[]string{"-pv"}, []string{"run", "-pv"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := InjectRun(tc.in); !slices.Equal(got, tc.want) {
				t.Errorf("InjectRun(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
