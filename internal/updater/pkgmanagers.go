package updater

// DetectPackageManagers returns the Linux package managers present on this
// host, in priority order (highest first). Detection is capability-based
// (is the binary on PATH) rather than parsing /etc/os-release, since the
// binary's presence is exactly the capability needed to run the install
// command anyway.
func DetectPackageManagers(lookPath func(string) (string, error)) []PackageManager {
	candidates := []PackageManager{
		{
			Name:        "dpkg",
			AssetSuffix: ".deb",
			InstallArgs: func(p string) []string { return []string{"-i", p} },
		},
		{
			Name:        "rpm",
			AssetSuffix: ".rpm",
			// -U (upgrade) rather than -i (install): -i refuses when a
			// same-named package at a different version is already
			// installed, which is exactly the state every update-after-the-
			// first-install is in. -U installs-or-upgrades correctly either way.
			InstallArgs: func(p string) []string { return []string{"-U", p} },
		},
		{
			Name:        "pacman",
			AssetSuffix: ".pkg.tar.zst",
			// --noconfirm avoids a second, redundant "Proceed with
			// installation? [Y/n]" prompt after omac's own confirmation.
			InstallArgs: func(p string) []string { return []string{"-U", "--noconfirm", p} },
		},
		{
			Name:        "apk",
			AssetSuffix: ".apk",
			InstallArgs: func(p string) []string { return []string{"add", "--allow-untrusted", p} },
		},
	}

	var out []PackageManager
	for _, c := range candidates {
		if _, err := lookPath(c.Name); err == nil {
			out = append(out, c)
		}
	}
	return out
}
