package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qooqapp/qooq/pkg/buildctx"
)

// maxUploadBytes is the most qooq Cloud accepts, compressed. The server
// enforces it too; checking here means a too-big directory is refused before
// the first byte leaves the machine.
const maxUploadBytes = 100 << 20

// uploadLimit is what is actually compared against; a var only so a test can
// lower it instead of building a 100 MB archive.
var uploadLimit int64 = maxUploadBytes

// The build service's other two limits (internal/watcher/untar.go), checked
// here for the same reason: a project that compresses well but unpacks huge —
// text data, a vendored dependency tree — used to upload in full and then be
// refused with a message about the compressed size it was well under. Vars so
// tests can lower them.
var (
	unpackedLimit  int64 = 500 << 20
	fileCountLimit       = 50_000
)

// Directories and files left out of a directory that is not a git repository.
// In a git repository .gitignore decides instead, which is the rule people
// already know.
var defaultExcludedDirs = map[string]bool{
	".git": true, "node_modules": true, ".venv": true, "__pycache__": true,
}

var defaultExcludedFiles = map[string]bool{".DS_Store": true}

// fileSet is what `qooq deploy` is about to send.
type fileSet struct {
	Files []string // slash-separated, relative to the directory, sorted
	// EnvSkipped are the .env* files that were left out on purpose.
	EnvSkipped []string
}

// isEnvFile: a secrets file by convention. Anything whose basename starts with
// ".env" — .env, .env.local, .env.production — and, knowingly, .env.example
// too: the rule is one a person can hold in their head.
func isEnvFile(rel string) bool {
	return strings.HasPrefix(filepath.Base(rel), ".env")
}

func isGitWorkTree(dir string) bool {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree").Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// listFiles decides what gets uploaded.
func listFiles(dir string) (*fileSet, error) {
	var candidates []string
	var err error
	if isGitWorkTree(dir) {
		candidates, err = gitFiles(dir)
	} else {
		candidates, err = walkFiles(dir)
	}
	if err != nil {
		return nil, err
	}

	var ign *ignoreRules
	if f, err := os.Open(filepath.Join(dir, ".qooqignore")); err == nil {
		ign = parseIgnore(f)
		f.Close()
	}

	set := &fileSet{}
	seen := map[string]bool{}
	for _, rel := range candidates {
		if seen[rel] {
			continue
		}
		seen[rel] = true
		// A tracked file that has since been deleted is still listed by git.
		if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			continue
		}
		if ign.Ignored(rel) {
			continue
		}
		if isEnvFile(rel) {
			set.EnvSkipped = append(set.EnvSkipped, rel)
			continue
		}
		set.Files = append(set.Files, rel)
	}
	sort.Strings(set.Files)
	sort.Strings(set.EnvSkipped)
	return set, nil
}

// gitFiles: tracked plus untracked-but-not-ignored. This is "what .gitignore
// lets through", so a node_modules the project already ignores never goes up.
func gitFiles(dir string) ([]string, error) {
	out, err := exec.Command("git", "-C", dir, "ls-files", "-co", "--exclude-standard", "-z").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w", dir, err)
	}
	var files []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 {
			files = append(files, filepath.ToSlash(string(p)))
		}
	}
	return files, nil
}

func walkFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && defaultExcludedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if defaultExcludedFiles[d.Name()] {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		// Symlinks (to files or directories alike) are listed as themselves;
		// WalkDir does not follow them.
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}

// packed is a tar.gz on disk, ready to send.
type packed struct {
	Path       string
	Compressed int64
	Raw        int64
	Count      int
	Largest    []fileSize
}

type fileSize struct {
	Path string
	Size int64
}

// checkBeforePacking refuses a file set over the unpacked-size or file-count
// limit, before any time is spent compressing it. The message lists what to
// exclude: the largest files for size, the fullest directories for count.
func checkBeforePacking(dir string, files []string) error {
	if len(files) > fileCountLimit {
		counts := map[string]int{}
		for _, f := range files {
			top := f
			if i := strings.Index(f, "/"); i > 0 {
				top = f[:i] + "/"
			}
			counts[top]++
		}
		type dc struct {
			dir string
			n   int
		}
		var list []dc
		for d, n := range counts {
			list = append(list, dc{d, n})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].n > list[j].n })
		if len(list) > 5 {
			list = list[:5]
		}
		var b strings.Builder
		fmt.Fprintf(&b, "the upload has %d files, over the %d limit. Where they are:\n", len(files), fileCountLimit)
		for _, d := range list {
			fmt.Fprintf(&b, "  %8d  %s\n", d.n, d.dir)
		}
		b.WriteString("exclude what the app does not need to run (dependencies are installed during the build) by adding it to a .qooqignore file")
		return errors.New(b.String())
	}
	var raw int64
	var sizes []fileSize
	for _, rel := range files {
		if info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel))); err == nil && info.Mode().IsRegular() {
			raw += info.Size()
			sizes = append(sizes, fileSize{rel, info.Size()})
		}
	}
	if raw <= unpackedLimit {
		return nil
	}
	sort.Slice(sizes, func(i, j int) bool { return sizes[i].Size > sizes[j].Size })
	if len(sizes) > 5 {
		sizes = sizes[:5]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "the upload is %s before compression, over the %s limit. Largest files:\n", humanSize(raw), humanSize(unpackedLimit))
	for _, f := range sizes {
		fmt.Fprintf(&b, "  %10s  %s\n", humanSize(f.Size), f.Path)
	}
	b.WriteString("exclude what the app does not need to run by adding it to a .qooqignore file")
	return errors.New(b.String())
}

// pack writes the files into a temporary tar.gz. The caller removes Path.
func pack(dir string, files []string) (*packed, error) {
	tmp, err := os.CreateTemp("", "qooq-deploy-*.tar.gz")
	if err != nil {
		return nil, err
	}
	p := &packed{Path: tmp.Name(), Count: len(files)}
	ok := false
	defer func() {
		if !ok {
			os.Remove(p.Path)
		}
	}()

	for _, rel := range files {
		if info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel))); err == nil && info.Mode().IsRegular() {
			p.Raw += info.Size()
			p.Largest = append(p.Largest, fileSize{rel, info.Size()})
		}
	}
	sort.Slice(p.Largest, func(i, j int) bool { return p.Largest[i].Size > p.Largest[j].Size })
	if len(p.Largest) > 5 {
		p.Largest = p.Largest[:5]
	}

	gz := gzip.NewWriter(tmp)
	if err := buildctx.TarFiles(dir, files, gz); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := gz.Close(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	info, err := os.Stat(p.Path)
	if err != nil {
		return nil, err
	}
	p.Compressed = info.Size()
	ok = true
	return p, nil
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// tooLargeMessage names the biggest files, because "too large" alone leaves
// the person to find out which directory they forgot to exclude.
func (p *packed) tooLargeMessage() string {
	var b strings.Builder
	fmt.Fprintf(&b, "the upload is %s compressed, over the %s limit. Largest files:\n", humanSize(p.Compressed), humanSize(uploadLimit))
	for _, f := range p.Largest {
		fmt.Fprintf(&b, "  %10s  %s\n", humanSize(f.Size), f.Path)
	}
	b.WriteString("exclude what the app does not need to run by adding it to a .qooqignore file")
	return b.String()
}
