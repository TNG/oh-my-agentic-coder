package buildrun

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// traceEnabled gates the temporary IT-leg diagnostic traces added for
// PR #221 (the TestE2EJvmBuild it-leg-loop "Could not find a valid
// Docker environment" investigation). The traces print to the build
// stderr (relayed through the broker to the captured e2e output) so a
// single CI run shows exactly where the container-proxy / DOCKER_HOST
// path breaks. Off by default so unit tests that assert exact stderr
// output are unaffected; the e2e-build.yml IT leg exports
// OMAC_BUILD_TRACE=1.
//
// TEMPORARY: revert once the root cause is pinned.
var traceEnabled = os.Getenv("OMAC_BUILD_TRACE") == "1"

// tracef writes a diagnostic line to w when OMAC_BUILD_TRACE=1.
func tracef(w io.Writer, format string, args ...any) {
	if !traceEnabled || w == nil {
		return
	}
	if !strings.HasSuffix(format, "\n") {
		format += "\n"
	}
	fmt.Fprintf(w, "omac build: "+format, args...)
}
