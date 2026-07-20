//go:build !linux

package origin

func platformResolver() Resolver { return noopResolver{} }
