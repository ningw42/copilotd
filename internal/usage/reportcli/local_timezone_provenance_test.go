package reportcli

import (
	"fmt"
	"testing"
)

func TestCommandRejectsForbiddenRootProvenance(t *testing.T) {
	for _, subtree := range []string{"right", "posix"} {
		for _, fromTZ := range []bool{false, true} {
			for _, layout := range []string{"direct-root-alias", "relocated-reserved-directory", "parent-directory-alias"} {
				t.Run(fmt.Sprintf("%s/%s/absoluteTZ=%t", subtree, layout, fromTZ), func(t *testing.T) {
					env := map[string]string{}
					files := newTimezoneFiles(t, "linux", env)
					reserved := "/nix/store/provenance-tzdata/share/zoneinfo/" + subtree
					var selected string
					switch layout {
					case "direct-root-alias":
						files.zone(reserved + "/Europe/Berlin")
						files.link("/usr/share/zoneinfo", reserved)
						selected = "/usr/share/zoneinfo/Europe/Berlin"
					case "relocated-reserved-directory":
						files.zone("/data/tz/Europe/Berlin")
						files.link(reserved, "/data/tz")
						files.link("/usr/share/zoneinfo", reserved)
						selected = "/data/tz/Europe/Berlin"
					case "parent-directory-alias":
						files.zone(reserved + "/zoneinfo/Europe/Berlin")
						files.link("/usr/share", reserved)
						selected = "/usr/share/zoneinfo/Europe/Berlin"
					}
					if fromTZ {
						env["TZ"] = selected
					} else {
						files.link("/etc/localtime", selected)
					}
					assertLocalTimezone(t, files, "")
				})
			}
		}
	}
}

func TestCommandRejectsNormalizedForbiddenRootProvenance(t *testing.T) {
	for _, subtree := range []string{"right", "posix"} {
		t.Run(subtree, func(t *testing.T) {
			files := newTimezoneFiles(t, "linux", nil)
			base := "/nix/store/provenance-tzdata/share/zoneinfo"
			files.zone(base + "/tz/Europe/Berlin")
			files.file(base+"/"+subtree+"/.keep", nil)
			files.link("/usr/share/zoneinfo", base+"//./"+subtree+"/../tz")
			files.link("/etc/localtime", base+"/tz/Europe/Berlin")
			assertLocalTimezone(t, files, "")
		})
	}
}

func TestCommandRejectsForbiddenSubtreeBelowResolvedRoot(t *testing.T) {
	for _, subtree := range []string{"right", "posix"} {
		t.Run(subtree, func(t *testing.T) {
			files := newTimezoneFiles(t, "linux", nil)
			files.zone("/selected/Europe/Berlin")
			files.link("/data/tz/"+subtree, "/selected")
			files.link("/usr/share/zoneinfo", "/data/tz")
			files.link("/usr/share/lib/zoneinfo", "/data/tz/"+subtree)
			files.link("/etc/localtime", "/selected/Europe/Berlin")
			assertLocalTimezone(t, files, "")
		})
	}
}

func TestCommandRejectsForbiddenDuplicateResolvedRoot(t *testing.T) {
	for _, subtree := range []string{"right", "posix"} {
		t.Run(subtree, func(t *testing.T) {
			files := newTimezoneFiles(t, "linux", nil)
			reserved := "/nix/store/provenance-tzdata/share/zoneinfo/" + subtree
			files.zone("/data/tz/Europe/Berlin")
			files.link(reserved, "/data/tz")
			files.link("/usr/share/zoneinfo", reserved)
			files.link("/usr/share/lib/zoneinfo", "/data/tz")
			files.link("/etc/localtime", "/data/tz/Europe/Berlin")
			assertLocalTimezone(t, files, "")
		})
	}
}

func TestCommandAllowsUnrelatedRootProvenance(t *testing.T) {
	for _, subtree := range []string{"right", "posix"} {
		for _, layout := range []string{"unrelated-parent", "directory-parent-alias", "zoneinfo-backup"} {
			t.Run(subtree+"/"+layout, func(t *testing.T) {
				files := newTimezoneFiles(t, "linux", nil)
				var root string
				switch layout {
				case "unrelated-parent":
					root = "/srv/" + subtree + "/tzdata/zoneinfo"
					files.zone(root + "/Europe/Berlin")
					files.link("/usr/share/zoneinfo", root)
				case "directory-parent-alias":
					root = "/srv/" + subtree + "/usr/share/zoneinfo"
					files.zone(root + "/Europe/Berlin")
					files.link("/usr/share", "/srv/"+subtree+"/usr/share")
				case "zoneinfo-backup":
					root = "/srv/zoneinfo-backup/" + subtree + "/tzdb"
					files.zone(root + "/Europe/Berlin")
					files.link("/usr/share/zoneinfo", root)
				}
				files.link("/etc/localtime", root+"/Europe/Berlin")
				assertLocalTimezone(t, files, "Europe/Berlin")
			})
		}
	}
}

func TestCommandAllowsUnrelatedForbiddenRoot(t *testing.T) {
	for _, subtree := range []string{"right", "posix"} {
		t.Run(subtree, func(t *testing.T) {
			files := newTimezoneFiles(t, "linux", nil)
			reserved := "/nix/store/provenance-tzdata/share/zoneinfo/" + subtree
			files.zone(reserved + "/Europe/Berlin")
			files.link("/usr/share/zoneinfo", reserved)
			files.zone("/etc/zoneinfo/Europe/Berlin")
			files.link("/etc/localtime", "/etc/zoneinfo/Europe/Berlin")
			assertLocalTimezone(t, files, "Europe/Berlin")
		})
	}
}
