package keychain

import "os/user"

// userCurrent returns the current OS username. Split out so tests can reason
// about it independently of the security(1) wrapper.
func userCurrent() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Username, nil
}
