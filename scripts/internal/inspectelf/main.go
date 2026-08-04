package main

import (
	"debug/elf"
	"flag"
	"fmt"
	"os"
)

func main() {
	path := flag.String("file", "", "ELF file to inspect")
	expectedMachine := flag.String("machine", "", "expected machine: amd64 or arm64")
	flag.Parse()
	if *path == "" || *expectedMachine == "" {
		fatalf("-file and -machine are required")
	}

	file, err := elf.Open(*path)
	if err != nil {
		fatalf("open %s: %v", *path, err)
	}
	defer file.Close()

	want := map[string]elf.Machine{
		"amd64": elf.EM_X86_64,
		"arm64": elf.EM_AARCH64,
	}[*expectedMachine]
	if want == elf.EM_NONE {
		fatalf("unsupported expected machine %q", *expectedMachine)
	}
	if file.Machine != want {
		fatalf("%s machine is %s, expected %s", *path, file.Machine, want)
	}
	for _, program := range file.Progs {
		if program.Type == elf.PT_INTERP {
			fatalf("%s contains a dynamic interpreter", *path)
		}
	}
	libraries, err := file.ImportedLibraries()
	if err != nil {
		fatalf("inspect dynamic dependencies: %v", err)
	}
	if len(libraries) != 0 {
		fatalf("%s has dynamic dependencies: %v", *path, libraries)
	}
	fmt.Printf("%s: ELF %s %s, static\n", *path, file.Class, file.Machine)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "inspectelf: "+format+"\n", args...)
	os.Exit(1)
}
