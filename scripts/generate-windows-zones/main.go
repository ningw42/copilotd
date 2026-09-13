// Command generate-windows-zones deterministically generates the runtime CLDR
// Windows-zone candidate table from the pinned vendored XML. It is offline: it
// never downloads or resolves a tag.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const dataDirectory = "internal/usage/reportcli/windowszonesdata"

type identityRecord struct {
	Release string `json:"release"`
	Tag     string `json:"tag"`
	Commit  string `json:"commit"`
	Source  struct {
		Path   string `json:"path"`
		Size   int    `json:"size"`
		SHA256 string `json:"sha256"`
	} `json:"source"`
}

type supplementalData struct {
	WindowsZones struct {
		MapTimezones struct {
			OtherVersion string    `xml:"otherVersion,attr"`
			TypeVersion  string    `xml:"typeVersion,attr"`
			Zones        []mapZone `xml:"mapZone"`
		} `xml:"mapTimezones"`
	} `xml:"windowsZones"`
}

type mapZone struct {
	Key       string `xml:"other,attr"`
	Territory string `xml:"territory,attr"`
	Type      string `xml:"type,attr"`
}

func main() {
	check := flag.Bool("check", false, "fail unless generated output is current")
	flag.Parse()
	if flag.NArg() != 0 {
		fatalf("unexpected operands")
	}
	root, err := repositoryRoot()
	if err != nil {
		fatalf("%v", err)
	}
	identityPath := filepath.Join(root, dataDirectory, "identity.json")
	sourcePath := filepath.Join(root, dataDirectory, "windowsZones.xml")
	outputPath := filepath.Join(root, dataDirectory, "zones_generated.go")

	identityBytes, err := os.ReadFile(identityPath)
	if err != nil {
		fatalf("read identity: %v", err)
	}
	var identity identityRecord
	if err := json.Unmarshal(identityBytes, &identity); err != nil {
		fatalf("decode identity: %v", err)
	}
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		fatalf("read source: %v", err)
	}
	sum := sha256.Sum256(source)
	if len(source) != identity.Source.Size || hex.EncodeToString(sum[:]) != identity.Source.SHA256 {
		fatalf("pinned source identity mismatch: size=%d sha256=%s", len(source), hex.EncodeToString(sum[:]))
	}
	generated, err := generate(identity, source)
	if err != nil {
		fatalf("generate: %v", err)
	}
	if *check {
		current, err := os.ReadFile(outputPath)
		if err != nil {
			fatalf("read generated output: %v", err)
		}
		if !bytes.Equal(current, generated) {
			fatalf("%s is stale; run go generate ./internal/usage/reportcli/windowszonesdata", filepath.ToSlash(outputPath))
		}
		fmt.Printf("CLDR Windows-zone generated data is current (%s, sha256:%s)\n", identity.Tag, identity.Source.SHA256)
		return
	}
	if err := os.WriteFile(outputPath, generated, 0644); err != nil {
		fatalf("write generated output: %v", err)
	}
	fmt.Printf("generated %s from %s (%s)\n", filepath.ToSlash(outputPath), identity.Source.Path, identity.Tag)
}

func repositoryRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", errors.New("could not find repository go.mod")
		}
		directory = parent
	}
}

func generate(identity identityRecord, source []byte) ([]byte, error) {
	if identity.Release != "48" || identity.Tag != "release-48" || len(identity.Commit) != 40 || identity.Source.Path != "common/supplemental/windowsZones.xml" {
		return nil, errors.New("identity is not the approved CLDR release-48 source")
	}
	var document supplementalData
	decoder := xml.NewDecoder(bytes.NewReader(source))
	decoder.Strict = true
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	metadata := document.WindowsZones.MapTimezones
	if metadata.OtherVersion == "" || metadata.TypeVersion == "" || len(metadata.Zones) == 0 {
		return nil, errors.New("missing CLDR Windows-zone metadata or mappings")
	}
	mappings := make(map[string]map[string][]string)
	for index, zone := range metadata.Zones {
		candidates := strings.Fields(zone.Type)
		if zone.Key == "" || zone.Territory == "" || len(candidates) == 0 {
			return nil, fmt.Errorf("mapping %d has an empty key, territory, or candidate list", index)
		}
		if zone.Territory != "001" && !alpha2(zone.Territory) {
			return nil, fmt.Errorf("mapping %d has malformed territory %q", index, zone.Territory)
		}
		for _, candidate := range candidates {
			if !slashName(candidate) {
				return nil, fmt.Errorf("mapping %d has unsupported candidate %q", index, candidate)
			}
		}
		byTerritory := mappings[zone.Key]
		if byTerritory == nil {
			byTerritory = make(map[string][]string)
			mappings[zone.Key] = byTerritory
		}
		if _, duplicate := byTerritory[zone.Territory]; duplicate {
			return nil, fmt.Errorf("duplicate mapping for %q/%q", zone.Key, zone.Territory)
		}
		byTerritory[zone.Territory] = append([]string(nil), candidates...)
	}
	for key, byTerritory := range mappings {
		if len(byTerritory["001"]) == 0 {
			return nil, fmt.Errorf("Windows key %q has no 001 mapping", key)
		}
	}

	var output strings.Builder
	fmt.Fprintln(&output, "// Code generated by go run ./scripts/generate-windows-zones; DO NOT EDIT.")
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "package windowszonesdata")
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "const (")
	fmt.Fprintf(&output, "\tCLDRRelease = %q\n", identity.Release)
	fmt.Fprintf(&output, "\tCLDRTag = %q\n", identity.Tag)
	fmt.Fprintf(&output, "\tCLDRCommit = %q\n", identity.Commit)
	fmt.Fprintf(&output, "\tSourceSHA256 = %q\n", identity.Source.SHA256)
	fmt.Fprintf(&output, "\tWindowsOtherVersion = %q\n", metadata.OtherVersion)
	fmt.Fprintf(&output, "\tTZDBTypeVersion = %q\n", metadata.TypeVersion)
	fmt.Fprintln(&output, ")")
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "var generatedCandidates = map[string]map[string][]string{")
	keys := sortedKeys(mappings)
	for _, key := range keys {
		fmt.Fprintf(&output, "\t%q: {\n", key)
		territories := sortedKeys(mappings[key])
		for _, territory := range territories {
			fmt.Fprintf(&output, "\t\t%q: {", territory)
			for index, candidate := range mappings[key][territory] {
				if index != 0 {
					fmt.Fprint(&output, ", ")
				}
				fmt.Fprintf(&output, "%q", candidate)
			}
			fmt.Fprintln(&output, "},")
		}
		fmt.Fprintln(&output, "\t},")
	}
	fmt.Fprintln(&output, "}")
	formatted, err := format.Source([]byte(output.String()))
	if err != nil {
		return nil, fmt.Errorf("format generated Go: %w", err)
	}
	return formatted, nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func alpha2(value string) bool {
	return len(value) == 2 && value[0] >= 'A' && value[0] <= 'Z' && value[1] >= 'A' && value[1] <= 'Z'
}

func slashName(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, character := range part {
			if !(character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '-' || character == '+' || character == '.') {
				return false
			}
		}
	}
	return true
}

func fatalf(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, "generate-windows-zones: "+format+"\n", arguments...)
	os.Exit(1)
}
