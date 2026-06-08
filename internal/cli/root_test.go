package cli

import "testing"

func TestBareAction(t *testing.T) {
	cases := map[string]struct {
		initialized bool
		accounts    int
		tty         bool
		want        rootAction
	}{
		"populated pool shows status":          {true, 2, true, actionStatus},
		"populated pool shows status non-tty":  {true, 1, false, actionStatus},
		"uninitialized tty onboards":           {false, 0, true, actionAdd},
		"initialized but empty tty onboards":   {true, 0, true, actionAdd},
		"uninitialized non-tty fails loud":     {false, 0, false, actionErr},
		"initialized empty non-tty fails loud": {true, 0, false, actionErr},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := bareAction(tc.initialized, tc.accounts, tc.tty); got != tc.want {
				t.Errorf("bareAction(%v, %d, %v) = %v, want %v",
					tc.initialized, tc.accounts, tc.tty, got, tc.want)
			}
		})
	}
}
