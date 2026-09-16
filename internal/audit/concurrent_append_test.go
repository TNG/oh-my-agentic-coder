package audit

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestConcurrentAppendProducesWellFormedLines exercises the O_APPEND atomicity
// guarantee: two goroutines writing capped records to the same file concurrently
// must produce only valid, parseable JSON Lines — no interleaving within a record.
//
// Each record is built at the maxPathBytes cap (the largest any FacadeRequest
// path can be after truncation), so the marshalled size is close to the
// maximum that the write path will ever produce. On Linux, a single write(2)
// to an O_APPEND file is atomic up to PIPE_BUF (4096 bytes), so records under
// that limit from concurrent writers cannot be split.
func TestConcurrentAppendProducesWellFormedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.jsonl")

	const writers = 2
	const eventsEach = 500

	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			a, err := New(Config{
				Enabled: true,
				Path:    path,
				Mode:    ModeStart,
				RunID:   "run_concurrent",
			})
			if err != nil {
				t.Errorf("writer %d: New: %v", id, err)
				return
			}
			defer a.Close()
			// Use a path at exactly the cap to exercise maximum record size.
			longPath := "/" + strings.Repeat("x", maxPathBytes)
			for range eventsEach {
				a.Emit(FacadeRequest("GET", "m", "ns", longPath, 200, 0, 1))
			}
		}(i)
	}
	wg.Wait()

	events, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile returned error (corrupt records detected): %v", err)
	}
	want := writers * eventsEach
	if len(events) != want {
		t.Errorf("concurrent append: got %d parseable events, want %d — some records were lost or corrupted", len(events), want)
	}
}
