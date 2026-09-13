package windowszonesdata

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/usage/report"
)

func TestCLDRRelease48IdentityMatchesVendoredSourceAndLicense(t *testing.T) {
	identity := Identity()
	if identity.Repository != "https://github.com/unicode-org/cldr" || identity.Release != "48" || identity.Tag != "release-48" ||
		identity.Commit != "acd6d88ae493633240e19a87a721076a8a75c310" || identity.Tree != "bafae8fc919257506cb84327781ce4912b9b0c0b" ||
		identity.Source.Path != "common/supplemental/windowsZones.xml" || identity.Source.GitBlob != "26a62c3f0645851fa13ebddbce9b6f20528d5777" ||
		identity.Source.Size != 49378 || identity.Source.SHA256 != "9cf3db6a31fb382fee21b70be6feba1e82766b0fcd06e6261fb7936a73e537ff" ||
		identity.License.Path != "LICENSE" || identity.License.GitBlob != "861b74f3c812088755fd185d964e82a244503bbd" || identity.License.Size != 2033 ||
		identity.License.SHA256 != "b4c0ae8ef04f7059f96ce5bbe0467f9fe6f6d81bbe13517701dfeb961fb4d0b6" || identity.License.SPDX != "Unicode-3.0" {
		t.Fatalf("pinned identity = %+v", identity)
	}
	if CLDRRelease != identity.Release || CLDRTag != identity.Tag || CLDRCommit != identity.Commit || SourceSHA256 != identity.Source.SHA256 || WindowsOtherVersion != "7e11800" || TZDBTypeVersion != "2021a" {
		t.Fatalf("generated identity release=%q tag=%q commit=%q source=%q other=%q tzdb=%q", CLDRRelease, CLDRTag, CLDRCommit, SourceSHA256, WindowsOtherVersion, TZDBTypeVersion)
	}
	for _, artifact := range []struct {
		name string
		want int
		hash string
	}{
		{name: "windowsZones.xml", want: identity.Source.Size, hash: identity.Source.SHA256},
		{name: "LICENSE", want: identity.License.Size, hash: identity.License.SHA256},
	} {
		data, err := os.ReadFile(artifact.name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if len(data) != artifact.want || hex.EncodeToString(sum[:]) != artifact.hash {
			t.Fatalf("%s identity: size=%d sha256=%s", artifact.name, len(data), hex.EncodeToString(sum[:]))
		}
	}
	if !strings.Contains(string(License()), "SPDX-License-Identifier: Unicode-3.0") {
		t.Fatal("vendored license is not the pinned Unicode License V3 text")
	}
}

func TestCLDRRelease48CandidatesPreserveSourceOrder(t *testing.T) {
	got := Candidates("Central Standard Time", "US")
	want := []string{
		"America/Chicago",
		"America/Indiana/Knox",
		"America/Indiana/Tell_City",
		"America/Menominee",
		"America/North_Dakota/Beulah",
		"America/North_Dakota/Center",
		"America/North_Dakota/New_Salem",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Central Standard Time/US candidates = %v, want %v", got, want)
	}
	got[0] = "mutation"
	if again := Candidates("Central Standard Time", "US"); len(again) != len(want) || again[0] != want[0] {
		t.Fatalf("caller changed generated data: %v", again)
	}
}

func TestCLDRRelease48RepresentativeDefaultsAndAbsentMappings(t *testing.T) {
	for _, tc := range []struct {
		key, territory string
		want           []string
	}{
		{key: "Central Standard Time", territory: "001", want: []string{"America/Chicago"}},
		{key: "China Standard Time", territory: "001", want: []string{"Asia/Shanghai"}},
		{key: "China Standard Time", territory: "US"},
		{key: "Nepal Standard Time", territory: "001", want: []string{"Asia/Katmandu"}},
		{key: "Unknown Standard Time", territory: "001"},
		{key: "", territory: "001"},
	} {
		if got := Candidates(tc.key, tc.territory); fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("Candidates(%q, %q) = %v, want %v", tc.key, tc.territory, got, tc.want)
		}
	}
}

func TestCLDRRelease48EveryGeneratedCandidateLoadsThroughSharedValidator(t *testing.T) {
	seen := map[string]bool{}
	for key, byTerritory := range generatedCandidates {
		if len(byTerritory["001"]) == 0 {
			t.Errorf("%q has no world default", key)
		}
		for territory, candidates := range byTerritory {
			if len(candidates) == 0 {
				t.Errorf("%q/%q has no candidates", key, territory)
			}
			for _, candidate := range candidates {
				if seen[candidate] {
					continue
				}
				seen[candidate] = true
				if _, err := report.LoadTimezone(candidate); err != nil {
					t.Errorf("generated candidate %q is not accepted by report.LoadTimezone: %v", candidate, err)
				}
			}
		}
	}
	if len(generatedCandidates) != 139 || len(seen) != 445 {
		t.Fatalf("generated mapping inventory: Windows keys=%d distinct IANA names=%d", len(generatedCandidates), len(seen))
	}
}

func TestCLDRRelease48GenerationIsReproducibleOffline(t *testing.T) {
	command := exec.Command("go", "run", "../../../../scripts/generate-windows-zones", "-check")
	command.Env = append(os.Environ(), "GOPROXY=off")
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "release-48") || !strings.Contains(string(output), SourceSHA256) {
		t.Fatalf("offline generator check: %v\n%s", err, output)
	}
}
