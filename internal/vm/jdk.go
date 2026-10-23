package vm

import (
	"fmt"
	"regexp"
	"slices"
)

// jdkPattern matches the vm.yaml jdk names: "temurin-25" or the apt
// package name "temurin-25-jdk".
var jdkPattern = regexp.MustCompile(`^temurin-([1-9][0-9]{0,2})(-jdk)?$`)

// JDKPackages maps vm.yaml jdk names to the apt packages from the Adoptium
// repository: "temurin-25" and "temurin-25-jdk" both become
// "temurin-25-jdk". Duplicates are dropped, order is kept.
func JDKPackages(jdks []string) ([]string, error) {
	var pkgs []string
	for _, jdk := range jdks {
		m := jdkPattern.FindStringSubmatch(jdk)
		if m == nil {
			return nil, fmt.Errorf("omac vm: unsupported jdk %q (supported: temurin-<version>)", jdk)
		}
		if pkg := "temurin-" + m[1] + "-jdk"; !slices.Contains(pkgs, pkg) {
			pkgs = append(pkgs, pkg)
		}
	}
	return pkgs, nil
}
