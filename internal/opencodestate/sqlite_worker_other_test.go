//go:build !darwin && !linux

package opencodestate

func prepareSQLiteReplacement(string, string) error { return errInvalidSource }
func runSQLiteEntropyContract(string) int           { return 1 }
