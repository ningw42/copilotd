// Command artifactcheck reports executable format, architecture, and imported
// libraries using only Go's standard-library object readers.
package main

import (
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: artifactcheck EXECUTABLE...")
		os.Exit(2)
	}
	for _, path := range os.Args[1:] {
		if file, err := elf.Open(path); err == nil {
			libraries, libErr := file.ImportedLibraries()
			file.Close()
			if libErr != nil {
				fatal(path, libErr)
			}
			fmt.Printf("%s: format=ELF machine=%s imported_libraries=%q\n", path, file.Machine, libraries)
			continue
		}
		if file, err := macho.Open(path); err == nil {
			libraries, libErr := file.ImportedLibraries()
			file.Close()
			if libErr != nil {
				fatal(path, libErr)
			}
			fmt.Printf("%s: format=Mach-O cpu=%s imported_libraries=%q\n", path, file.Cpu, libraries)
			continue
		}
		if file, err := pe.Open(path); err == nil {
			libraries, libErr := file.ImportedLibraries()
			file.Close()
			if libErr != nil {
				fatal(path, libErr)
			}
			fmt.Printf("%s: format=PE machine=%#x imported_libraries=%q\n", path, file.Machine, libraries)
			continue
		}
		fatal(path, fmt.Errorf("unsupported executable format"))
	}
}

func fatal(path string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
	os.Exit(1)
}
