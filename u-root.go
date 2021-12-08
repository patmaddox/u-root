// Copyright 2015-2018 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	gbb "github.com/u-root/gobusybox/src/pkg/bb"
	gbbgolang "github.com/u-root/gobusybox/src/pkg/golang"
	"github.com/u-root/u-root/pkg/golang"
	"github.com/u-root/u-root/pkg/shlex"
	"github.com/u-root/u-root/pkg/uroot"
	"github.com/u-root/u-root/pkg/uroot/builder"
	"github.com/u-root/u-root/pkg/uroot/initramfs"
)

// multiFlag is used for flags that support multiple invocations, e.g. -files
type multiFlag []string

func (m *multiFlag) String() string {
	return fmt.Sprint(*m)
}

func (m *multiFlag) Set(value string) error {
	*m = append(*m, value)
	return nil
}

// Flags for u-root builder.
var (
	build, format, tmpDir, base, outputPath *string
	uinitCmd, initCmd                       *string
	defaultShell                            *string
	useExistingInit                         *bool
	noCommands                              *bool
	extraFiles                              multiFlag
	noStrip                                 *bool
	statsOutputPath                         *string
	statsLabel                              *string
	shellbang                               *bool
	tags                                    *string
	// For the new gobusybox support
	usegobusybox *bool
	genDir       *string
)

func init() {
	var sh string
	switch golang.Default().GOOS {
	case "plan9":
		sh = ""
	default:
		sh = "elvish"
	}

	build = flag.String("build", "bb", "u-root build format (e.g. bb or binary).")
	format = flag.String("format", "cpio", "Archival format.")

	tmpDir = flag.String("tmpdir", "", "Temporary directory to put binaries in.")

	base = flag.String("base", "", "Base archive to add files to. By default, this is a couple of directories like /bin, /etc, etc. u-root has a default internally supplied set of files; use base=/dev/null if you don't want any base files.")
	useExistingInit = flag.Bool("useinit", false, "Use existing init from base archive (only if --base was specified).")
	outputPath = flag.String("o", "", "Path to output initramfs file.")

	initCmd = flag.String("initcmd", "init", "Symlink target for /init. Can be an absolute path or a u-root command name. Use initcmd=\"\" if you don't want the symlink.")
	uinitCmd = flag.String("uinitcmd", "", "Symlink target and arguments for /bin/uinit. Can be an absolute path or a u-root command name. Use uinitcmd=\"\" if you don't want the symlink. E.g. -uinitcmd=\"echo foobar\"")
	defaultShell = flag.String("defaultsh", sh, "Default shell. Can be an absolute path or a u-root command name. Use defaultsh=\"\" if you don't want the symlink.")

	noCommands = flag.Bool("nocmd", false, "Build no Go commands; initramfs only")

	flag.Var(&extraFiles, "files", "Additional files, directories, and binaries (with their ldd dependencies) to add to archive. Can be speficified multiple times.")

	noStrip = flag.Bool("no-strip", false, "Build unstripped binaries")
	shellbang = flag.Bool("shellbang", false, "Use #! instead of symlinks for busybox")

	statsOutputPath = flag.String("stats-output-path", "", "Write build stats to this file (JSON)")
	statsLabel = flag.String("stats-label", "", "Use this statsLabel when writing stats")

	tags = flag.String("tags", "", "Comma separated list of build tags")

	// Flags for the gobusybox, which we hope to move to, since it works with modules.
	usegobusybox = flag.Bool("gobusybox", os.Getenv("GO111MODULE") != "off", "Use the new gobusybox package to build u-root")
	genDir = flag.String("gen-dir", "", "Directory to generate source in")

}

type buildStats struct {
	Label      string  `json:"label,omitempty"`
	Time       int64   `json:"time"`
	Duration   float64 `json:"duration"`
	OutputSize int64   `json:"output_size"`
}

func writeBuildStats(stats buildStats, path string) error {
	var allStats []buildStats
	if data, err := os.ReadFile(*statsOutputPath); err == nil {
		json.Unmarshal(data, &allStats)
	}
	found := false
	for i, s := range allStats {
		if s.Label == stats.Label {
			allStats[i] = stats
			found = true
			break
		}
	}
	if !found {
		allStats = append(allStats, stats)
		sort.Slice(allStats, func(i, j int) bool {
			return strings.Compare(allStats[i].Label, allStats[j].Label) == -1
		})
	}
	data, err := json.MarshalIndent(allStats, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*statsOutputPath, data, 0o644); err != nil {
		return err
	}
	return nil
}

func generateLabel() string {
	var baseCmds []string
	env := golang.Default()
	if len(flag.Args()) > 0 {
		// Use the last component of the name to keep the label short
		for _, e := range flag.Args() {
			baseCmds = append(baseCmds, path.Base(e))
		}
	} else {
		baseCmds = []string{"core"}
	}
	return fmt.Sprintf("%s-%s-%s-%s", *build, env.GOOS, env.GOARCH, strings.Join(baseCmds, "_"))
}

func gobusyboxMain() error {
	bopts := &gbbgolang.BuildOpts{}
	bopts.RegisterFlags(flag.CommandLine)

	o, err := filepath.Abs(*outputPath)
	if err != nil {
		return err
	}

	env := gbbgolang.Default()
	if env.CgoEnabled {
		log.Printf("Disabling CGO for u-root...")
		env.CgoEnabled = false
	}
	log.Printf("Build environment: %s", env)

	tmpDir := *genDir
	remove := false
	if tmpDir == "" {
		tdir, err := ioutil.TempDir("", "bb-")
		if err != nil {
			return fmt.Errorf("Could not create busybox source directory: %v", err)
		}
		tmpDir = tdir
		remove = true
	}

	if len(flag.Args()) == 0 {
		return fmt.Errorf("commands must be provided, as a path or glob, e.g., u-root cmds/core/* cmds/exp/rush")
	}
	opts := &gbb.Opts{
		Env:          env,
		GenSrcDir:    tmpDir,
		CommandPaths: flag.Args(),
		BinaryPath:   o,
		GoBuildOpts:  bopts,
	}
	if err := gbb.BuildBusybox(opts); err != nil {
		log.Print(err)
		var errGopath *gbb.ErrGopathBuild
		var errGomod *gbb.ErrModuleBuild
		if errors.As(err, &errGopath) {
			return fmt.Errorf("preserving bb generated source directory at %s due to error. To reproduce build, `cd %s` and `GO111MODULE=off GOPATH=%s go build`", tmpDir, errGopath.CmdDir, errGopath.GOPATH)
		} else if errors.As(err, &errGomod) {
			return fmt.Errorf("preserving bb generated source directory at %s due to error. To debug build, `cd %s` and use `go build` to build, or `go mod [why|tidy|graph]` to debug dependencies, or `go list -m all` to list all dependency versions", tmpDir, errGomod.CmdDir)
		} else {
			return fmt.Errorf("preserving bb generated source directory at %s due to error", tmpDir)
		}
	}
	// Only remove temp dir if there was no error.
	if remove {
		os.RemoveAll(tmpDir)
	}
	return nil
}

func main() {
	flag.Parse()

	start := time.Now()

	if *usegobusybox {
		if err := gobusyboxMain(); err != nil {
			log.Fatal(err)
		}
		return
	}
	// Main is in a separate functions so defers run on return.
	if err := Main(); err != nil {
		log.Fatal(err)
	}

	elapsed := time.Now().Sub(start)

	stats := buildStats{
		Label:    *statsLabel,
		Time:     start.Unix(),
		Duration: float64(elapsed.Milliseconds()) / 1000,
	}
	if stats.Label == "" {
		stats.Label = generateLabel()
	}
	if stat, err := os.Stat(*outputPath); err == nil && stat.ModTime().After(start) {
		log.Printf("Successfully built %q (size %d).", *outputPath, stat.Size())
		stats.OutputSize = stat.Size()
		if *statsOutputPath != "" {
			if err := writeBuildStats(stats, *statsOutputPath); err == nil {
				log.Printf("Wrote stats to %q (label %q)", *statsOutputPath, stats.Label)
			} else {
				log.Printf("Failed to write stats to %s: %v", *statsOutputPath, err)
			}
		}
	}
}

var recommendedVersions = []string{
	"go1.17",
}

func isRecommendedVersion(v string) bool {
	for _, r := range recommendedVersions {
		if strings.HasPrefix(v, r) {
			return true
		}
	}
	return false
}

// Main is a separate function so defers are run on return, which they wouldn't
// on exit.
func Main() error {
	env := golang.Default()
	env.BuildTags = strings.Split(*tags, ",")
	if env.CgoEnabled {
		log.Printf("Disabling CGO for u-root...")
		env.CgoEnabled = false
	}
	log.Printf("Build environment: %s", env)
	if env.GOOS != "linux" {
		log.Printf("GOOS is not linux. Did you mean to set GOOS=linux?")
	}

	v, err := env.Version()
	if err != nil {
		log.Printf("Could not get environment's Go version, using runtime's version: %v", err)
		v = runtime.Version()
	}
	if !isRecommendedVersion(v) {
		log.Printf(`WARNING: You are not using one of the recommended Go versions (have = %s, recommended = %v).
			Some packages may not compile.
			Go to https://golang.org/doc/install to find out how to install a newer version of Go,
			or use https://godoc.org/golang.org/dl/%s to install an additional version of Go.`,
			v, recommendedVersions, recommendedVersions[0])
	}

	archiver, err := initramfs.GetArchiver(*format)
	if err != nil {
		return err
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)
	// Open the target initramfs file.
	if *outputPath == "" {
		if len(env.GOOS) == 0 && len(env.GOARCH) == 0 {
			return fmt.Errorf("passed no path, GOOS, and GOARCH to CPIOArchiver.OpenWriter")
		}
		*outputPath = fmt.Sprintf("/tmp/initramfs.%s_%s.cpio", env.GOOS, env.GOARCH)
	}
	w, err := archiver.OpenWriter(logger, *outputPath)
	if err != nil {
		return err
	}

	var baseFile initramfs.Reader
	if *base != "" {
		bf, err := os.Open(*base)
		if err != nil {
			return err
		}
		defer bf.Close()
		baseFile = archiver.Reader(bf)
	} else {
		baseFile = uroot.DefaultRamfs().Reader()
	}

	tempDir := *tmpDir
	if tempDir == "" {
		var err error
		tempDir, err = os.MkdirTemp("", "u-root")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tempDir)
	} else if _, err := os.Stat(tempDir); os.IsNotExist(err) {
		if err := os.MkdirAll(tempDir, 0o755); err != nil {
			return fmt.Errorf("temporary directory %q did not exist; tried to mkdir but failed: %v", tempDir, err)
		}
	}

	var (
		c           []uroot.Commands
		initCommand = *initCmd
	)
	if !*noCommands {
		var b builder.Builder
		switch *build {
		case "bb":
			b = builder.BBBuilder{ShellBang: *shellbang}
		case "binary":
			b = builder.BinaryBuilder{}
		case "source":
			return fmt.Errorf("source mode has been deprecated")
		default:
			return fmt.Errorf("could not find builder %q", *build)
		}

		// Resolve globs into package imports.
		//
		// Currently allowed formats:
		//   Go package imports; e.g. github.com/u-root/u-root/cmds/ls (must be in $GOPATH)
		//   Paths to Go package directories; e.g. $GOPATH/src/github.com/u-root/u-root/cmds/*
		var pkgs []string
		for _, a := range flag.Args() {
			p, ok := templates[a]
			if !ok {
				pkgs = append(pkgs, a)
				continue
			}
			pkgs = append(pkgs, p...)
		}
		if len(pkgs) == 0 {
			pkgs = []string{"github.com/u-root/u-root/cmds/core/*"}
		}

		// The command-line tool only allows specifying one build mode
		// right now.
		c = append(c, uroot.Commands{
			Builder:  b,
			Packages: pkgs,
		})
	}

	opts := uroot.Opts{
		Env:             env,
		Commands:        c,
		TempDir:         tempDir,
		ExtraFiles:      extraFiles,
		OutputFile:      w,
		BaseArchive:     baseFile,
		UseExistingInit: *useExistingInit,
		InitCmd:         initCommand,
		DefaultShell:    *defaultShell,
		NoStrip:         *noStrip,
	}
	uinitArgs := shlex.Argv(*uinitCmd)
	if len(uinitArgs) > 0 {
		opts.UinitCmd = uinitArgs[0]
	}
	if len(uinitArgs) > 1 {
		opts.UinitArgs = uinitArgs[1:]
	}
	return uroot.CreateInitramfs(logger, opts)
}
