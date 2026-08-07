package netprompt

import (
	"sync"
	"testing"
)

func TestSessionStoreLookupExact(t *testing.T) {
	s := NewSessionStore()
	if err := s.Record("example.com", "host", true); err != nil {
		t.Fatalf("Record: %v", err)
	}

	allow, found := s.Lookup("example.com")
	if !found || !allow {
		t.Fatalf("Lookup(example.com) = (%v, %v), want (true, true)", allow, found)
	}
	if _, found := s.Lookup("sub.example.com"); found {
		t.Fatalf("Lookup(sub.example.com) found a host-scope match, want not found")
	}
	if _, found := s.Lookup("other.com"); found {
		t.Fatalf("Lookup(other.com) found a match, want not found")
	}
}

func TestSessionStoreLookupSuffix(t *testing.T) {
	s := NewSessionStore()
	if err := s.Record("example.com", "suffix", true); err != nil {
		t.Fatalf("Record: %v", err)
	}

	for _, host := range []string{"example.com", "sub.example.com", "a.b.example.com"} {
		allow, found := s.Lookup(host)
		if !found || !allow {
			t.Fatalf("Lookup(%s) = (%v, %v), want (true, true)", host, allow, found)
		}
	}
	if _, found := s.Lookup("notexample.com"); found {
		t.Fatalf("Lookup(notexample.com) found a suffix match, want not found")
	}
}

func TestSessionStoreDenyWins(t *testing.T) {
	s := NewSessionStore()
	// Allow the suffix, deny the exact host: deny must win.
	if err := s.Record("example.com", "suffix", true); err != nil {
		t.Fatalf("Record allow suffix: %v", err)
	}
	if err := s.Record("deny.example.com", "host", false); err != nil {
		t.Fatalf("Record deny host: %v", err)
	}

	allow, found := s.Lookup("deny.example.com")
	if !found || allow {
		t.Fatalf("Lookup(deny.example.com) = (%v, %v), want (false, true)", allow, found)
	}

	// Now a broad suffix deny overriding an earlier exact allow.
	s2 := NewSessionStore()
	if err := s2.Record("example.com", "host", true); err != nil {
		t.Fatalf("Record allow host: %v", err)
	}
	if err := s2.Record("example.com", "suffix", false); err != nil {
		t.Fatalf("Record deny suffix: %v", err)
	}
	allow, found = s2.Lookup("example.com")
	if !found || allow {
		t.Fatalf("Lookup(example.com) with deny suffix = (%v, %v), want (false, true)", allow, found)
	}
}

func TestSessionStoreRecordUpsert(t *testing.T) {
	s := NewSessionStore()
	if err := s.Record("example.com", "host", true); err != nil {
		t.Fatalf("Record allow: %v", err)
	}
	allow, found := s.Lookup("example.com")
	if !found || !allow {
		t.Fatalf("after allow, Lookup = (%v, %v), want (true, true)", allow, found)
	}

	// Same host+scope flips the decision in place.
	if err := s.Record("example.com", "host", false); err != nil {
		t.Fatalf("Record deny: %v", err)
	}
	allow, found = s.Lookup("example.com")
	if !found || allow {
		t.Fatalf("after upsert to deny, Lookup = (%v, %v), want (false, true)", allow, found)
	}

	// A different scope is a distinct entry, not an upsert.
	if err := s.Record("example.com", "suffix", true); err != nil {
		t.Fatalf("Record allow suffix: %v", err)
	}
	allow, found = s.Lookup("example.com")
	if !found || allow {
		t.Fatalf("host-scope deny must still win over suffix allow, got (%v, %v)", allow, found)
	}

	if err := s.Record("example.com", "bogus", true); err == nil {
		t.Fatalf("Record with invalid scope succeeded, want error")
	}
}

func TestSessionStoreConcurrent(t *testing.T) {
	s := NewSessionStore()
	const workers = 32
	const iterations = 200

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			host := "host.example.com"
			if w%2 == 1 {
				host = "sub.host.example.com"
				_ = s.Record("host.example.com", "suffix", w%3 != 0)
			} else {
				_ = s.Record("host.example.com", "host", w%3 != 0)
			}
			for i := 0; i < iterations; i++ {
				_, _ = s.Lookup(host)
			}
		}(w)
	}
	wg.Wait()

	if _, found := s.Lookup("host.example.com"); !found {
		t.Fatal("after concurrent Records, Lookup(host.example.com) found nothing")
	}
}
