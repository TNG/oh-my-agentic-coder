//go:build !darwin && !linux

package opencodestate

func installSQLiteWorkerGuard() error { return errInvalidSource }
