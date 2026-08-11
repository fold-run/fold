package gateway

import "testing"

// resolveVersion is where the two distribution channels meet: goreleaser
// stamps releases, and the module proxy cannot stamp anything. The function
// takes the module lookup as an argument precisely so this can exercise every
// branch — debug.ReadBuildInfo() inside a test binary reports the test
// binary's own metadata and can never be made to say "v1.10.0".
func TestResolveVersion(t *testing.T) {
	const stampedRelease = "v1.10.0"

	tests := []struct {
		name    string
		stamped string
		module  string
		want    string
	}{
		{
			name:    "a stamped release keeps its stamp",
			stamped: stampedRelease,
			module:  "v0.0.1", // must not win; the linker is the authority
			want:    stampedRelease,
		},
		{
			name:    "go install records the tag it resolved",
			stamped: "dev",
			module:  "v1.10.0",
			want:    "v1.10.0",
		},
		{
			name:    "go install of a commit records a pseudo-version",
			stamped: "dev",
			module:  "v1.10.1-0.20260811193000-1f695ea80960",
			want:    "v1.10.1-0.20260811193000-1f695ea80960",
		},
		{
			name:    "a build from a working tree stays dev",
			stamped: "dev",
			module:  "(devel)",
			want:    "dev",
		},
		{
			name:    "a binary with no module metadata stays dev",
			stamped: "dev",
			module:  "",
			want:    "dev",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveVersion(tc.stamped, func() string { return tc.module })
			if got != tc.want {
				t.Errorf("resolveVersion(%q, %q) = %q, want %q", tc.stamped, tc.module, got, tc.want)
			}
		})
	}
}

// The whole point of the change: a proxy-installed gateway used to report
// "dev" to /api/federation, which the console reads as "unknowable, treat as
// current" — so every operator following the README's `go run …@latest`
// silently lost version-skew detection. Guard the specific case rather than
// only the table above, because this is the regression that matters.
func TestProxyInstallReportsATagRatherThanDev(t *testing.T) {
	got := resolveVersion("dev", func() string { return "v1.10.0" })
	if got == "dev" {
		t.Fatal("a go install build still reports dev; version-skew detection stays off for it")
	}
	if got != "v1.10.0" {
		t.Errorf("got %q, want the module version v1.10.0", got)
	}
}
