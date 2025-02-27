// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa_test

import (
	"bufio"
	"bytes"
	"flag"
	"internal/buildcfg"
	"runtime"
	"sort"

	"fmt"
	"internal/testenv"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"testing"
)

// Matches lines in genssa output that are marked "isstmt", and the parenthesized plus-prefixed line number is a submatch
var asmLine *regexp.Regexp = regexp.MustCompile(`^\s[vb][0-9]+\s+[0-9]+\s\(\+([0-9]+)\)`)

// this matches e.g.                            `   v123456789   000007   (+9876654310) MOVUPS	X15, ""..autotmp_2-32(SP)`

// Matches lines in genssa output that describe an inlined file.
// Note it expects an unadventurous choice of basename.
var sepRE = regexp.QuoteMeta(string(filepath.Separator))
var inlineLine *regexp.Regexp = regexp.MustCompile(`^#\s.*` + sepRE + `[-a-zA-Z0-9_]+\.go:([0-9]+)`)

// this matches e.g.                                 #  /pa/inline-dumpxxxx.go:6

var testGoArchFlag = flag.String("arch", "", "run test for specified architecture")

func testGoArch() string {
	if *testGoArchFlag == "" {
		return runtime.GOARCH
	}
	return *testGoArchFlag
}

func TestDebugLinesSayHi(t *testing.T) {
	// This test is potentially fragile, the goal is that debugging should step properly through "sayhi"
	// If the blocks are reordered in a way that changes the statement order but execution flows correctly,
	// then rearrange the expected numbers.  Register abi and not-register-abi also have different sequences,
	// at least for now.

	switch testGoArch() {
	case "arm64", "amd64": // register ABI
		testDebugLines(t, "-N -l", "sayhi.go", "sayhi", []int{8, 9, 10, 11}, false)

	case "arm", "386": // probably not register ABI for a while
		testDebugLines(t, "-N -l", "sayhi.go", "sayhi", []int{9, 10, 11}, false)

	default: // expect ppc64le and riscv will pick up register ABI soonish, not sure about others
		t.Skip("skipped for many architectures, also changes w/ register ABI")
	}
}

func TestDebugLinesPushback(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" { // in particular, it could be windows.
		t.Skip("this test depends on creating a file with a wonky name, only works for sure on Linux and Darwin")
	}

	switch testGoArch() {
	default:
		t.Skip("skipped for many architectures")

	case "arm64", "amd64": // register ABI
		fn := "(*List[go.shape.int_0]).PushBack"
		if buildcfg.Experiment.Unified {
			// Unified mangles differently
			fn = "(*List[int]).PushBack-shaped"
		}
		testDebugLines(t, "-N -l", "pushback.go", fn, []int{17, 18, 19, 20, 21, 22, 24}, true)
	}
}

func TestDebugLinesConvert(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" { // in particular, it could be windows.
		t.Skip("this test depends on creating a file with a wonky name, only works for sure on Linux and Darwin")
	}

	switch testGoArch() {
	default:
		t.Skip("skipped for many architectures")

	case "arm64", "amd64": // register ABI
		fn := "G[go.shape.int_0]"
		if buildcfg.Experiment.Unified {
			// Unified mangles differently
			fn = "G[int]-shaped"
		}
		testDebugLines(t, "-N -l", "convertline.go", fn, []int{9, 10, 11}, true)
	}
}

func TestInlineLines(t *testing.T) {
	if runtime.GOARCH != "amd64" && *testGoArchFlag == "" {
		// As of september 2021, works for everything except mips64, but still potentially fragile
		t.Skip("only runs for amd64 unless -arch explicitly supplied")
	}

	want := [][]int{{3}, {4, 10}, {4, 10, 16}, {4, 10}, {4, 11, 16}, {4, 11}, {4}, {5, 10}, {5, 10, 16}, {5, 10}, {5, 11, 16}, {5, 11}, {5}}
	testInlineStack(t, "inline-dump.go", "f", want)
}

func compileAndDump(t *testing.T, file, function, moreGCFlags string) []byte {
	testenv.MustHaveGoBuild(t)

	tmpdir, err := ioutil.TempDir("", "debug_lines_test")
	if err != nil {
		panic(fmt.Sprintf("Problem creating TempDir, error %v", err))
	}
	if testing.Verbose() {
		fmt.Printf("Preserving temporary directory %s\n", tmpdir)
	} else {
		defer os.RemoveAll(tmpdir)
	}

	source, err := filepath.Abs(filepath.Join("testdata", file))
	if err != nil {
		panic(fmt.Sprintf("Could not get abspath of testdata directory and file, %v", err))
	}

	cmd := exec.Command(testenv.GoToolPath(t), "build", "-o", "foo.o", "-gcflags=-d=ssa/genssa/dump="+function+" "+moreGCFlags, source)
	cmd.Dir = tmpdir
	cmd.Env = replaceEnv(cmd.Env, "GOSSADIR", tmpdir)
	testGoos := "linux" // default to linux
	if testGoArch() == "wasm" {
		testGoos = "js"
	}
	cmd.Env = replaceEnv(cmd.Env, "GOOS", testGoos)
	cmd.Env = replaceEnv(cmd.Env, "GOARCH", testGoArch())

	if testing.Verbose() {
		fmt.Printf("About to run %s\n", asCommandLine("", cmd))
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("error running cmd %s: %v\nstdout:\n%sstderr:\n%s\n", asCommandLine("", cmd), err, stdout.String(), stderr.String())
	}

	if s := stderr.String(); s != "" {
		t.Fatalf("Wanted empty stderr, instead got:\n%s\n", s)
	}

	dumpFile := filepath.Join(tmpdir, function+"_01__genssa.dump")
	dumpBytes, err := os.ReadFile(dumpFile)
	if err != nil {
		t.Fatalf("Could not read dump file %s, err=%v", dumpFile, err)
	}
	return dumpBytes
}

func sortInlineStacks(x [][]int) {
	sort.Slice(x, func(i, j int) bool {
		if len(x[i]) != len(x[j]) {
			return len(x[i]) < len(x[j])
		}
		for k := range x[i] {
			if x[i][k] != x[j][k] {
				return x[i][k] < x[j][k]
			}
		}
		return false
	})
}

// testInlineStack ensures that inlining is described properly in the comments in the dump file
func testInlineStack(t *testing.T, file, function string, wantStacks [][]int) {
	// this is an inlining reporting test, not an optimization test.  -N makes it less fragile
	dumpBytes := compileAndDump(t, file, function, "-N")
	dump := bufio.NewScanner(bytes.NewReader(dumpBytes))
	dumpLineNum := 0
	var gotStmts []int
	var gotStacks [][]int
	for dump.Scan() {
		line := dump.Text()
		dumpLineNum++
		matches := inlineLine.FindStringSubmatch(line)
		if len(matches) == 2 {
			stmt, err := strconv.ParseInt(matches[1], 10, 32)
			if err != nil {
				t.Fatalf("Expected to parse a line number but saw %s instead on dump line #%d, error %v", matches[1], dumpLineNum, err)
			}
			if testing.Verbose() {
				fmt.Printf("Saw stmt# %d for submatch '%s' on dump line #%d = '%s'\n", stmt, matches[1], dumpLineNum, line)
			}
			gotStmts = append(gotStmts, int(stmt))
		} else if len(gotStmts) > 0 {
			gotStacks = append(gotStacks, gotStmts)
			gotStmts = nil
		}
	}
	if len(gotStmts) > 0 {
		gotStacks = append(gotStacks, gotStmts)
		gotStmts = nil
	}
	sortInlineStacks(gotStacks)
	sortInlineStacks(wantStacks)
	if !reflect.DeepEqual(wantStacks, gotStacks) {
		t.Errorf("wanted inlines %+v but got %+v", wantStacks, gotStacks)
	}

}

// testDebugLines compiles testdata/<file> with flags -N -l and -d=ssa/genssa/dump=<function>
// then verifies that the statement-marked lines in that file are the same as those in wantStmts
// These files must all be short because this is super-fragile.
// "go build" is run in a temporary directory that is normally deleted, unless -test.v
func testDebugLines(t *testing.T, gcflags, file, function string, wantStmts []int, ignoreRepeats bool) {
	dumpBytes := compileAndDump(t, file, function, gcflags)
	dump := bufio.NewScanner(bytes.NewReader(dumpBytes))
	var gotStmts []int
	dumpLineNum := 0
	for dump.Scan() {
		line := dump.Text()
		dumpLineNum++
		matches := asmLine.FindStringSubmatch(line)
		if len(matches) == 2 {
			stmt, err := strconv.ParseInt(matches[1], 10, 32)
			if err != nil {
				t.Fatalf("Expected to parse a line number but saw %s instead on dump line #%d, error %v", matches[1], dumpLineNum, err)
			}
			if testing.Verbose() {
				fmt.Printf("Saw stmt# %d for submatch '%s' on dump line #%d = '%s'\n", stmt, matches[1], dumpLineNum, line)
			}
			gotStmts = append(gotStmts, int(stmt))
		}
	}
	if ignoreRepeats { // remove repeats from gotStmts
		newGotStmts := []int{gotStmts[0]}
		for _, x := range gotStmts {
			if x != newGotStmts[len(newGotStmts)-1] {
				newGotStmts = append(newGotStmts, x)
			}
		}
		if !reflect.DeepEqual(wantStmts, newGotStmts) {
			t.Errorf("wanted stmts %v but got %v (with repeats still in: %v)", wantStmts, newGotStmts, gotStmts)
		}

	} else {
		if !reflect.DeepEqual(wantStmts, gotStmts) {
			t.Errorf("wanted stmts %v but got %v", wantStmts, gotStmts)
		}
	}
}
