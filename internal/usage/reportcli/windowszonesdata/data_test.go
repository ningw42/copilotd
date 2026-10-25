package windowszonesdata

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/usage/report"
)

type testIdentityRecord struct {
	Repository string `json:"repository"`
	Release    string `json:"release"`
	Tag        string `json:"tag"`
	Commit     string `json:"commit"`
	Tree       string `json:"tree"`
	Source     struct {
		Path    string `json:"path"`
		GitBlob string `json:"git_blob"`
		Size    int    `json:"size"`
		SHA256  string `json:"sha256"`
	} `json:"source"`
	License struct {
		Path    string `json:"path"`
		GitBlob string `json:"git_blob"`
		Size    int    `json:"size"`
		SHA256  string `json:"sha256"`
		SPDX    string `json:"spdx"`
	} `json:"license"`
}

func readTestIdentity(t *testing.T) testIdentityRecord {
	t.Helper()
	raw, err := os.ReadFile("identity.json")
	if err != nil {
		t.Fatal(err)
	}
	var identity testIdentityRecord
	if err := json.Unmarshal(raw, &identity); err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestCLDRRelease49IdentityMatchesVendoredSourceAndLicense(t *testing.T) {
	identity := readTestIdentity(t)
	if identity.Repository != "https://github.com/unicode-org/cldr" || identity.Release != "49" || identity.Tag != "release-49" ||
		identity.Commit != "96e54b273d2da4ec6fbad350ba19ff23fa24a588" || identity.Tree != "bd84336f381c121e90534054a441c37fe0d9d500" ||
		identity.Source.Path != "common/supplemental/windowsZones.xml" || identity.Source.GitBlob != "2dc8c386cb3aed1a8c21b4ba7bb7b8d4f3535507" ||
		identity.Source.Size != 49892 || identity.Source.SHA256 != "5f902f94106a396cb7774f0b0e1ca4dfecb686494045350c0b4935d378e3913b" ||
		identity.License.Path != "LICENSE" || identity.License.GitBlob != "b4132054a5d2b95b6f1a4c8b511a211086023e55" || identity.License.Size != 3193 ||
		identity.License.SHA256 != "65eb33419e22a297550a811b5b535f5e532f9a7e0114b516c0ea18afd53c3804" || identity.License.SPDX != "Unicode-3.0" {
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
	license, err := os.ReadFile("LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(license), "SPDX-License-Identifier: Unicode-3.0") {
		t.Fatal("vendored license is not the pinned Unicode License V3 text")
	}
}

func TestCLDRRelease49CandidatesPreserveSourceOrder(t *testing.T) {
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

func TestCLDRRelease49RepresentativeDefaultsAndAbsentMappings(t *testing.T) {
	for _, tc := range []struct {
		key, territory string
		want           []string
	}{
		{key: "Central Standard Time", territory: "001", want: []string{"America/Chicago"}},
		{key: "British Columbia Standard Time", territory: "001", want: []string{"America/Vancouver"}},
		{key: "Alberta Standard Time", territory: "CA", want: []string{"America/Edmonton", "America/Inuvik"}},
		{key: "Manitoba Standard Time", territory: "001", want: []string{"America/Winnipeg"}},
		{key: "Mountain Standard Time", territory: "CA", want: []string{"America/Cambridge_Bay"}},
		{key: "Central Standard Time", territory: "CA", want: []string{"America/Rankin_Inlet", "America/Resolute"}},
		{key: "Pacific Standard Time", territory: "CA"},
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

func TestCLDRRelease49EveryGeneratedCandidateLoadsThroughSharedValidator(t *testing.T) {
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
	if len(generatedCandidates) != 142 || len(seen) != 445 {
		t.Fatalf("generated mapping inventory: Windows keys=%d distinct IANA names=%d", len(generatedCandidates), len(seen))
	}
}

func TestCLDRRelease49GenerationIsReproducibleOffline(t *testing.T) {
	command := exec.Command("go", "run", "../../../../scripts/generate-windows-zones", "-check")
	command.Env = append(os.Environ(), "GOPROXY=off")
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "release-49") || !strings.Contains(string(output), SourceSHA256) {
		t.Fatalf("offline generator check: %v\n%s", err, output)
	}
}
