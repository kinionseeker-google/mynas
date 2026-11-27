// Command mkpkg builds the OpenWrt packages for mynas: an .ipk (opkg, OpenWrt
// 24.10 and older) and an .apk (apk-tools 3, OpenWrt 25.12 and newer) for
// every supported CPU family.
//
// Run it from the repository root:
//
//	go run ./packaging/mkpkg -version 0.1.8
//
// The .ipk files are written by this program alone. The .apk files need the
// "apk mkpkg" tool, which is run in a container, so Docker must be available
// (use -apk=off to skip them).
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type target struct {
	cpu     string // name used in the package file name
	goarch  string
	goarm   string
	gomips  string
	pattern string // shell case pattern matched against DISTRIB_ARCH
}

// One static binary per CPU family covers every OpenWrt sub-architecture in it:
// soft-float MIPS and ARMv5 code runs on all the newer cores too.
var targets = []target{
	{cpu: "aarch64", goarch: "arm64", pattern: "aarch64*"},
	{cpu: "x86_64", goarch: "amd64", pattern: "x86_64*"},
	{cpu: "mipsel", goarch: "mipsle", gomips: "softfloat", pattern: "mipsel*"},
	{cpu: "mips", goarch: "mips", gomips: "softfloat", pattern: "mips_* | mips"},
	{cpu: "arm", goarch: "arm", goarm: "5", pattern: "arm_* | arm"},
}

const (
	pkgName     = "mynas"
	description = "Private NAS with a web file manager, WebDAV server, external links, offline downloads and WebDAV mounts"
	depends     = "ca-bundle"
	// apk-tools 3 lives in this image; the packages it writes install on OpenWrt 25.12.
	apkImage = "alpine:3.23"
)

var conffiles = []string{"/etc/config/mynas"}

type file struct {
	name string // slash-separated, no leading slash
	mode int64
	data []byte
}

func main() {
	version := flag.String("version", "0.1.8", "package version")
	release := flag.Int("release", 1, "package release number")
	out := flag.String("out", "dist", "output directory")
	only := flag.String("only", "", "comma-separated CPU names to build (default: all)")
	apkMode := flag.String("apk", "auto", "build .apk packages: auto (when Docker is available), on, off")
	flag.Parse()

	if _, err := os.Stat(filepath.Join("packaging", "root")); err != nil {
		log.Fatal("run mkpkg from the repository root (packaging/root not found)")
	}
	work := filepath.Join(*out, ".work")
	must(os.RemoveAll(work))
	must(os.MkdirAll(work, 0o755))

	shared := collectRoot(filepath.Join("packaging", "root"))
	now := time.Now()
	var built []target
	for _, t := range targets {
		if *only != "" && !contains(strings.Split(*only, ","), t.cpu) {
			continue
		}
		bin := buildBinary(t, *version, work)
		files := append([]file{{name: "usr/bin/mynas", mode: 0o755, data: bin}}, shared...)
		fill := strings.NewReplacer("@PATTERN@", t.pattern, "@CPU@", t.cpu)
		script := func(name string) []byte {
			return []byte(fill.Replace(string(readText(filepath.Join("packaging", "scripts", name)))))
		}
		scripts := map[string][]byte{"preinst": script("preinst"), "postinst": script("postinst"), "prerm": script("prerm")}

		ipk := filepath.Join(*out, fmt.Sprintf("%s_%s-%d_%s.ipk", pkgName, *version, *release, t.cpu))
		must(os.WriteFile(ipk, buildIPK(files, scripts, fmt.Sprintf("%s-%d", *version, *release), now), 0o644))
		fmt.Printf("%-8s %s (%d KB)\n", t.cpu, ipk, fileSize(ipk)/1024)

		// Staging for the apk step: the same tree plus the lists OpenWrt keeps under /lib/apk/packages.
		stage := filepath.Join(work, t.cpu)
		must(os.MkdirAll(stage, 0o755))
		apkFiles := append([]file{}, files...)
		var list strings.Builder
		for _, f := range apkFiles {
			list.WriteString("/" + f.name + "\n")
		}
		apkFiles = append(apkFiles,
			file{name: "lib/apk/packages/" + pkgName + ".list", mode: 0o644, data: []byte(list.String())},
			file{name: "lib/apk/packages/" + pkgName + ".conffiles", mode: 0o644, data: []byte(strings.Join(conffiles, "\n") + "\n")},
		)
		must(os.WriteFile(filepath.Join(stage, "data.tar"), tarOf(apkFiles, now), 0o644))
		// No pre-install script for apk: apk carries on when it fails, so the CPU
		// check lives in post-install instead.
		for name, apkName := range map[string]string{"postinst": "post-install", "prerm": "pre-deinstall"} {
			must(os.WriteFile(filepath.Join(stage, apkName), scripts[name], 0o644))
		}
		built = append(built, t)
	}
	if len(built) == 0 {
		log.Fatal("no target matched -only")
	}
	// The installer script and the user guide travel with the packages.
	for _, name := range []string{"install.sh", "使用与安装说明.md"} {
		must(os.WriteFile(filepath.Join(*out, name), readText(filepath.Join("packaging", name)), 0o644))
	}

	switch *apkMode {
	case "off":
	case "auto":
		if _, err := exec.LookPath("docker"); err != nil {
			fmt.Println("docker not found: skipped the .apk packages")
			break
		}
		fallthrough
	default:
		buildAPKs(*out, fmt.Sprintf("%s-r%d", *version, *release))
	}
	must(os.RemoveAll(work))
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if strings.TrimSpace(v) == s {
			return true
		}
	}
	return false
}

func fileSize(p string) int64 {
	info, err := os.Stat(p)
	must(err)
	return info.Size()
}

// readText reads a script or config file and forces Unix line endings: a
// stray CRLF from a Windows checkout would break the shell scripts on the router.
func readText(p string) []byte {
	b, err := os.ReadFile(p)
	must(err)
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// collectRoot loads packaging/root. Executable bits cannot be trusted on a
// Windows checkout, so modes come from where the file is installed.
func collectRoot(root string) []file {
	var files []file
	must(filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		mode := int64(0o644)
		for _, dir := range []string{"etc/init.d/", "etc/uci-defaults/", "etc/hotplug.d/", "usr/bin/"} {
			if strings.HasPrefix(name, dir) {
				mode = 0o755
			}
		}
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		files = append(files, file{name: name, mode: mode, data: readText(p)})
		return nil
	}))
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files
}

func buildBinary(t target, version, work string) []byte {
	out := filepath.Join(work, "mynas-"+t.cpu)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w -X main.version="+version, "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+t.goarch, "GOARM="+t.goarm, "GOMIPS="+t.gomips)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("building for %s: %v", t.cpu, err)
	}
	b, err := os.ReadFile(out)
	must(err)
	return b
}

// tarOf writes files, with their parent directories, as a root-owned tar archive.
func tarOf(files []file, mod time.Time) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := func(name string, mode int64, size int64, typ byte) {
		must(tw.WriteHeader(&tar.Header{
			Name: name, Mode: mode, Size: size, Typeflag: typ, ModTime: mod,
			Uid: 0, Gid: 0, Uname: "root", Gname: "root", Format: tar.FormatGNU,
		}))
	}
	hdr("./", 0o755, 0, tar.TypeDir)
	seen := map[string]bool{}
	sorted := append([]file{}, files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })
	for _, f := range sorted {
		var dirs []string
		for d := path.Dir(f.name); d != "." && d != "/"; d = path.Dir(d) {
			dirs = append([]string{d}, dirs...)
		}
		for _, d := range dirs {
			if !seen[d] {
				seen[d] = true
				hdr("./"+d+"/", 0o755, 0, tar.TypeDir)
			}
		}
		hdr("./"+f.name, f.mode, int64(len(f.data)), tar.TypeReg)
		_, err := tw.Write(f.data)
		must(err)
	}
	must(tw.Close())
	return buf.Bytes()
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	must(err)
	_, err = zw.Write(b)
	must(err)
	must(zw.Close())
	return buf.Bytes()
}

// buildIPK assembles an OpenWrt .ipk: a gzip-compressed tar holding
// debian-binary, data.tar.gz and control.tar.gz.
func buildIPK(files []file, scripts map[string][]byte, version string, mod time.Time) []byte {
	var installed int
	for _, f := range files {
		installed += len(f.data)
	}
	control := fmt.Sprintf("Package: %s\nVersion: %s\nDepends: libc, %s\nSection: net\nArchitecture: all\nInstalled-Size: %d\nDescription: %s\n",
		pkgName, version, depends, installed, description)
	ctl := []file{
		{name: "control", mode: 0o644, data: []byte(control)},
		{name: "conffiles", mode: 0o644, data: []byte(strings.Join(conffiles, "\n") + "\n")},
	}
	for name, body := range scripts {
		ctl = append(ctl, file{name: name, mode: 0o755, data: body})
	}
	outer := []file{
		{name: "debian-binary", mode: 0o644, data: []byte("2.0\n")},
		{name: "data.tar.gz", mode: 0o644, data: gz(tarOf(files, mod))},
		{name: "control.tar.gz", mode: 0o644, data: gz(tarOf(ctl, mod))},
	}
	// The outer archive keeps this fixed order and has no directory entries.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range outer {
		must(tw.WriteHeader(&tar.Header{Name: "./" + f.name, Mode: f.mode, Size: int64(len(f.data)), Typeflag: tar.TypeReg,
			ModTime: mod, Uname: "root", Gname: "root", Format: tar.FormatGNU}))
		_, err := tw.Write(f.data)
		must(err)
	}
	must(tw.Close())
	return gz(buf.Bytes())
}

// buildAPKs turns every staged tree under <out>/.work into an .apk with "apk mkpkg".
func buildAPKs(out, version string) {
	abs, err := filepath.Abs(out)
	must(err)
	script := `set -e
for d in /out/.work/*/; do
  cpu=$(basename "$d")
  [ -f "$d/data.tar" ] || continue
  rm -rf /tmp/root && mkdir /tmp/root
  tar -xf "$d/data.tar" -C /tmp/root
  apk mkpkg \
    --info "name:` + pkgName + `" --info "version:` + version + `" --info "arch:noarch" \
    --info "description:` + description + `" --info "origin:` + pkgName + `" --info "depends:` + depends + `" \
    --script "post-install:$d/post-install" --script "post-upgrade:$d/post-install" \
    --script "pre-deinstall:$d/pre-deinstall" \
    --files /tmp/root --output "/out/` + pkgName + `-` + version + `_$cpu.apk"
  echo "$cpu /out/` + pkgName + `-` + version + `_$cpu.apk"
done`
	cmd := exec.Command("docker", "run", "--rm", "-v", abs+":/out", apkImage, "sh", "-c", script)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("building the .apk packages failed: %v", err)
	}
}
