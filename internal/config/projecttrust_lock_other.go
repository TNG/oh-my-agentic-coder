//go:build !unix

package config

// lockProjectPins has no flock on non-unix platforms: the pin store write is
// still a single rename, so a torn store cannot result, only a lost update.
func lockProjectPins() (unlock func(), err error) {
	return func() {}, nil
}
