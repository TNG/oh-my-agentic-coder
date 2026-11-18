package sandboxprofile

// The platform baselines for tests in package sandboxprofile_test, which
// may import packages that import this one.
var (
	LinuxBaselineForTest  = linuxBaseline
	DarwinBaselineForTest = darwinBaseline
)
