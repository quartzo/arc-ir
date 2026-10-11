// Command arcc is the ARC compiler driver.
//
// Usage:
//
//	arcc verify   <file.arc>
//	arcc emit-asm <file.arc> [-o out.s]
//	arcc build    <file.arc> [-o name] [-emit-only] [-run] [-cc path]
//
// The pipeline is: parse -> verify (eBPF-style abstract interpretation) ->
// lower to x86_64 GNU assembly -> (optionally) assemble and link with the
// runtime -> (optionally) run.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/quartzo/arc-ir/pkg/codegen"
	"github.com/quartzo/arc-ir/pkg/ir"
	"github.com/quartzo/arc-ir/pkg/parser"
	"github.com/quartzo/arc-ir/pkg/runtime"
	"github.com/quartzo/arc-ir/pkg/verifier"
)

const version = "0.2.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "emit-asm":
		err = cmdEmitAsm(os.Args[2:])
	case "build":
		err = cmdBuild(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Printf("arcc %s\n", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "arcc: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "arcc: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `arcc - ARC (Arena Register Code) compiler

Usage:
  arcc verify   <file.arc>
  arcc emit-asm <file.arc> [-o out.s]
  arcc build    <file.arc> [-o name] [-emit-only] [-run] [-cc path]
  arcc version

Pipeline: parse -> verify -> x86_64 assembly -> assemble/link (optional) -> run
`)
}

func frontEnd(path string) (*ir.Program, *verifier.Result, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	prog, err := parser.Parse(string(src))
	if err != nil {
		return nil, nil, err
	}
	res, err := verifier.Verify(prog)
	if err != nil {
		return nil, nil, err
	}
	return prog, res, nil
}

func cmdVerify(args []string) error {
	flags, pos := splitArgs(args)
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	fs.Parse(flags)
	if len(pos) != 1 {
		return fmt.Errorf("usage: arcc verify <file.arc>")
	}
	prog, res, err := frontEnd(pos[0])
	if err != nil {
		return err
	}
	fmt.Printf("ok: %s verified (%d instructions, %d basic blocks)\n",
		pos[0], len(prog.Instrs), res.Blocks)
	return nil
}

func cmdEmitAsm(args []string) error {
	flags, pos := splitArgs(args)
	fs := flag.NewFlagSet("emit-asm", flag.ExitOnError)
	out := fs.String("o", "", "output assembly file (default stdout)")
	fs.Parse(flags)
	if len(pos) != 1 {
		return fmt.Errorf("usage: arcc emit-asm <file.arc> [-o out.s]")
	}
	_, res, err := frontEnd(pos[0])
	if err != nil {
		return err
	}
	asm, err := codegen.EmitAsm(res.Module)
	if err != nil {
		return err
	}
	if *out == "" {
		fmt.Print(asm)
		return nil
	}
	return os.WriteFile(*out, []byte(asm), 0o644)
}

func cmdBuild(args []string) error {
	flags, pos := splitArgs(args)
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	out := fs.String("o", "", "output base name (default: input without extension)")
	emitOnly := fs.Bool("emit-only", false, "only write the generated assembly, do not assemble")
	run := fs.Bool("run", false, "run the compiled program")
	cc := fs.String("cc", defaultCC(), "C compiler/assembler to invoke")
	fs.Parse(flags)
	if len(pos) != 1 {
		return fmt.Errorf("usage: arcc build <file.arc> [-o name] [-emit-only] [-run] [-cc path]")
	}
	input := pos[0]

	_, res, err := frontEnd(input)
	if err != nil {
		return err
	}
	asm, err := codegen.EmitAsm(res.Module)
	if err != nil {
		return err
	}

	base := *out
	if base == "" {
		base = strings.TrimSuffix(filepath.Base(input), filepath.Ext(input))
	}
	asmPath := base + ".s"
	rtPath := filepath.Join(dirOf(base), runtime.SourceName)
	if err := os.WriteFile(asmPath, []byte(asm), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(rtPath, []byte(runtime.Source), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s and %s\n", asmPath, rtPath)

	if *emitOnly {
		return nil
	}

	if err := runTool("assembler", *cc, "-O2", "-o", base, asmPath, rtPath); err != nil {
		return err
	}
	fmt.Printf("built %s\n", base)

	if *run {
		return runBinary(base)
	}
	return nil
}

func dirOf(path string) string {
	if d := filepath.Dir(path); d != "" {
		return d
	}
	return "."
}

func runTool(name string, bin string, args ...string) error {
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}

func runBinary(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	cmd := exec.Command(abs)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			fmt.Printf("program exited with status %d\n", ee.ExitCode())
			return nil
		}
		return err
	}
	return nil
}

func defaultCC() string {
	if cc := os.Getenv("CC"); cc != "" {
		return cc
	}
	return "cc"
}

// splitArgs separates flags from positional arguments so that the source file
// may appear anywhere on the command line. It knows which flags consume a
// following value (-o, -cc).
func splitArgs(args []string) (flags, positional []string) {
	valueFlags := map[string]bool{
		"-o": true, "--o": true,
		"-cc": true, "--cc": true,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && valueFlags[a] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	return flags, positional
}
