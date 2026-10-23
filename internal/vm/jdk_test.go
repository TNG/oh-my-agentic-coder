package vm

import (
	"slices"
	"strings"
	"testing"
)

func TestJDKPackagesMapsTemurinNames(t *testing.T) {
	got, err := JDKPackages([]string{"temurin-25", "temurin-21-jdk", "temurin-8", "temurin-25-jdk", "temurin-21"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"temurin-25-jdk", "temurin-21-jdk", "temurin-8-jdk"}
	if !slices.Equal(got, want) {
		t.Errorf("JDKPackages = %v, want %v", got, want)
	}
}

func TestJDKPackagesEmpty(t *testing.T) {
	if got, err := JDKPackages(nil); err != nil || len(got) != 0 {
		t.Errorf("JDKPackages(nil) = %v, %v; want empty, nil", got, err)
	}
}

func TestJDKPackagesRejectsUnsupported(t *testing.T) {
	for _, jdk := range []string{"25", "openjdk-21", "temurin", "temurin-", "temurin-1234", "temurin-0", "temurin-025", "temurin-007-jdk", "temurin-25-jre", "temurin-x", "Temurin-25", "temurin-25; reboot", ""} {
		_, err := JDKPackages([]string{jdk})
		if err == nil || !strings.Contains(err.Error(), "supported: temurin-<version>") {
			t.Errorf("JDKPackages(%q) err = %v, want unsupported error", jdk, err)
		}
	}
}
