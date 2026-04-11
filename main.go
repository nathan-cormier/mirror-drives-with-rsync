// Command mirror-rsync lists immediate subdirectories of a source root, lets you
// choose which to mirror to a destination root, then runs rsync -a --delete --progress
// for each selection.
// # Use -dry-run to run rsync in dry-run moedel, which is equivalent to running:
//
//	`rsync -n -i` (no changes, itemized output).
//
// # Use -resume to continue a partial sync (it uses .mirror-rsync-state.json on both the source and destination roots)
// After a full successful run the file is kept with nextIndex equal to the job length so -resume can
// detect completion.
//
// # Use -compare to print file counts and total sizes for source and
// destination roots without syncing; this is useful to confirm the mirroring completed as expected.
// It will also show you a diff of files that are only on the source or destination.
//
// Note: this will skip Apple sidecar files (they are not synced) that start with `._`,
// which macOS generates for certain filesystems (exFAT for example).
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

const stateFileName = ".mirror-rsync-state.json"

// errMissingState is returned when neither state file exists.
type errMissingState struct {
	srcPath, dstPath string
}

func (e errMissingState) Error() string {
	return fmt.Sprintf("no saved sync state at %s or %s", e.srcPath, e.dstPath)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	dryRun := flag.Bool("dry-run", false, "run rsync with -n and -i (no file changes; itemized output)")
	resume := flag.Bool("resume", false, "continue a partial sync using saved state on source and dest")
	compare := flag.Bool("compare", false, "print file count and total size for source and dest, and diff contents, then exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [-dry-run] [-resume] [-compare] <source_root> <dest_root>\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	args := flag.Args()
	if len(args) != 2 {
		return fmt.Errorf("usage: %s [-dry-run] [-resume] [-compare] <source_root> <dest_root>", filepath.Base(os.Args[0]))
	}
	if *dryRun && *resume {
		return fmt.Errorf("-dry-run cannot be used with -resume")
	}

	sourceRoot, err := absPath(args[0])
	if err != nil {
		return fmt.Errorf("source root: %w", err)
	}
	destRoot, err := absPath(args[1])
	if err != nil {
		return fmt.Errorf("dest root: %w", err)
	}

	if st, err := os.Stat(sourceRoot); err != nil {
		return fmt.Errorf("source root: %w", err)
	} else if !st.IsDir() {
		return fmt.Errorf("source root is not a directory: %s", sourceRoot)
	}
	if st, err := os.Stat(destRoot); err != nil {
		return fmt.Errorf("dest root: %w", err)
	} else if !st.IsDir() {
		return fmt.Errorf("dest root is not a directory: %s", destRoot)
	}

	if *compare {
		return runCompare(sourceRoot, destRoot)
	}

	rsyncPath, err := exec.LookPath("rsync")
	if err != nil {
		return fmt.Errorf("rsync not found in PATH: %w", err)
	}
	verLine, err := rsyncVersionLine(rsyncPath)
	if err != nil {
		return err
	}
	fmt.Println(verLine)
	if *dryRun {
		fmt.Println("Mode: dry run (rsync -n -i)")
	}
	if *resume {
		fmt.Println("Mode: resume")
	}
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)

	var folderNames []string
	var startIndex int

	if *resume {
		st, err := loadJobState(sourceRoot, destRoot)
		if err != nil {
			return err
		}
		if err := verifyStateMatches(st, sourceRoot, destRoot); err != nil {
			return err
		}
		folderNames = st.FolderNames
		startIndex = st.NextIndex
		if startIndex < 0 || startIndex > len(folderNames) {
			return fmt.Errorf("invalid saved progress in %s or %s (next index %d)", stateFilePath(sourceRoot), stateFilePath(destRoot), startIndex)
		}
		if startIndex == len(folderNames) {
			fmt.Println("Last run successful, nothing to resume.")
			return nil
		}
		for _, name := range folderNames {
			src := filepath.Join(sourceRoot, name)
			if err := requireDir(src); err != nil {
				return fmt.Errorf("resume: %w", err)
			}
		}

		done := folderNames[:startIndex]
		todo := folderNames[startIndex:]
		fmt.Printf("I already synced these folders: %s\n", formatFolderList(done))
		fmt.Printf("I will resume with these folders: %s\n", formatFolderList(todo))
		fmt.Print("Continue? [y/N] ")
		confirm, err := readLine(reader)
		if err != nil {
			return err
		}
		c := strings.ToLower(strings.TrimSpace(confirm))
		if c != "y" && c != "yes" {
			fmt.Println("Aborted.")
			return nil
		}
	} else {
		dirs, err := immediateSubdirs(sourceRoot)
		if err != nil {
			return err
		}
		if len(dirs) == 0 {
			fmt.Printf("No subdirectories found under: %s\n", sourceRoot)
			return nil
		}

		destDirs, err := immediateSubdirs(destRoot)
		if err != nil {
			return err
		}
		srcSet := make(map[string]bool, len(dirs))
		for _, d := range dirs {
			srcSet[filepath.Base(d)] = true
		}
		var destOnly []string
		for _, d := range destDirs {
			name := filepath.Base(d)
			if !srcSet[name] {
				destOnly = append(destOnly, name)
			}
		}

		fmt.Printf("Subdirectories on source: %s\n", sourceRoot)
		fmt.Printf("Destination root: %s\n\n", destRoot)
		for i, d := range dirs {
			fmt.Printf("  %2d) %s\n", i+1, filepath.Base(d))
		}
		if len(destOnly) > 0 {
			fmt.Println()
			for _, name := range destOnly {
				fmt.Printf("\033[33m   *) %s  (exists only on destination)\033[0m\n", name)
			}
		}
		fmt.Println()
		fmt.Println(`Enter numbers to mirror (space or comma separated), or "all" for every subdirectory.`)

		fmt.Print("Selection: ")
		line, err := readLine(reader)
		if err != nil {
			return err
		}

		chosen := parseSelection(line, len(dirs))
		if len(chosen) == 0 {
			fmt.Println("Nothing selected. Exiting.")
			return nil
		}

		unique := dedupeIndices(chosen)
		folderNames = folderNamesFromSelection(unique, dirs)

		fmt.Println()
		fmt.Println("Will sync:")
		for _, name := range folderNames {
			fmt.Printf("  %s -> %s\n", filepath.Join(sourceRoot, name), filepath.Join(destRoot, name))
		}
		fmt.Print("Proceed? [y/N] ")
		confirm, err := readLine(reader)
		if err != nil {
			return err
		}
		c := strings.ToLower(strings.TrimSpace(confirm))
		if c != "y" && c != "yes" {
			fmt.Println("Aborted.")
			return nil
		}

		if !*dryRun {
			if err := saveJobState(sourceRoot, destRoot, &jobState{
				Version:     1,
				SourceRoot:  sourceRoot,
				DestRoot:    destRoot,
				FolderNames: append([]string(nil), folderNames...),
				NextIndex:   0,
			}); err != nil {
				return err
			}
		}
		startIndex = 0
	}

	if err := runSyncBatch(rsyncPath, *dryRun, sourceRoot, destRoot, folderNames, startIndex); err != nil {
		return err
	}

	fmt.Println()
	if *dryRun {
		fmt.Println("Done. (dry run — no changes made)")
	} else {
		fmt.Println("Done.")
	}
	return nil
}

func runSyncBatch(rsyncPath string, dryRun bool, sourceRoot, destRoot string, folderNames []string, startIndex int) error {
	for i := startIndex; i < len(folderNames); i++ {
		name := folderNames[i]
		src := filepath.Join(sourceRoot, name)
		if err := requireDir(src); err != nil {
			return err
		}
		dest := filepath.Join(destRoot, name)
		if !dryRun {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", dest, err)
			}
		}
		fmt.Println()
		if dryRun {
			fmt.Printf(">>> rsync [dry-run]: %s/  ->  %s/\n", src, dest)
		} else {
			fmt.Printf(">>> rsync: %s/  ->  %s/\n", src, dest)
		}
		cmd := exec.Command(rsyncPath, rsyncMirrorArgs(dryRun, src, dest)...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("rsync %s: %w", name, err)
		}
		if !dryRun {
			next := i + 1
			if err := saveJobState(sourceRoot, destRoot, &jobState{
				Version:     1,
				SourceRoot:  sourceRoot,
				DestRoot:    destRoot,
				FolderNames: append([]string(nil), folderNames...),
				NextIndex:   next,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

type jobState struct {
	Version     int      `json:"version"`
	SourceRoot  string   `json:"sourceRoot"`
	DestRoot    string   `json:"destRoot"`
	FolderNames []string `json:"folderNames"`
	NextIndex   int      `json:"nextIndex"`
}

func stateFilePath(root string) string {
	return filepath.Join(root, stateFileName)
}

func loadJobState(sourceRoot, destRoot string) (*jobState, error) {
	srcPath := stateFilePath(sourceRoot)
	dstPath := stateFilePath(destRoot)
	dataSrc, errSrc := os.ReadFile(srcPath)
	dataDst, errDst := os.ReadFile(dstPath)
	if errSrc != nil && !os.IsNotExist(errSrc) {
		return nil, errSrc
	}
	if errDst != nil && !os.IsNotExist(errDst) {
		return nil, errDst
	}
	srcMissing := errSrc != nil && os.IsNotExist(errSrc)
	dstMissing := errDst != nil && os.IsNotExist(errDst)
	if srcMissing && dstMissing {
		return nil, errMissingState{srcPath: srcPath, dstPath: dstPath}
	}
	if srcMissing && !dstMissing {
		return parseJobState(dataDst, dstPath)
	}
	if dstMissing && !srcMissing {
		return parseJobState(dataSrc, srcPath)
	}
	stSrc, err := parseJobState(dataSrc, srcPath)
	if err != nil {
		return nil, err
	}
	stDst, err := parseJobState(dataDst, dstPath)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(*stSrc, *stDst) {
		return nil, fmt.Errorf("state file mismatch between %s and %s", srcPath, dstPath)
	}
	return stSrc, nil
}

func parseJobState(data []byte, path string) (*jobState, error) {
	var st jobState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if st.Version != 1 {
		return nil, fmt.Errorf("unsupported state version %d in %s", st.Version, path)
	}
	return &st, nil
}

func verifyStateMatches(st *jobState, sourceRoot, destRoot string) error {
	if st.SourceRoot != sourceRoot {
		return fmt.Errorf("saved state source root does not match (state has %q, you gave %q)", st.SourceRoot, sourceRoot)
	}
	if st.DestRoot != destRoot {
		return fmt.Errorf("saved state dest root does not match (state has %q, you gave %q)", st.DestRoot, destRoot)
	}
	return nil
}

func saveJobState(sourceRoot, destRoot string, st *jobState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(stateFilePath(sourceRoot), data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", stateFilePath(sourceRoot), err)
	}
	if err := os.WriteFile(stateFilePath(destRoot), data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", stateFilePath(destRoot), err)
	}
	return nil
}

func absPath(p string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return "", err
	}
	return abs, nil
}

func formatFolderList(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

func folderNamesFromSelection(unique []int, dirs []string) []string {
	out := make([]string, 0, len(unique))
	for _, idx := range unique {
		out = append(out, filepath.Base(dirs[idx]))
	}
	return out
}

func requireDir(p string) error {
	st, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("not a directory: %s", p)
	}
	return nil
}

func runCompare(sourceRoot, destRoot string) error {
	fmt.Printf("Scanning source: %s\n", sourceRoot)
	srcCount, srcSize, srcFiles, err := dirStats(sourceRoot)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	fmt.Println()
	fmt.Printf("Scanning dest:   %s\n", destRoot)
	dstCount, dstSize, dstFiles, err := dirStats(destRoot)
	if err != nil {
		return fmt.Errorf("dest: %w", err)
	}
	fmt.Println()
	fmt.Printf("Source: %d files, %s\n", srcCount, formatBytes(srcSize))
	fmt.Printf("Dest:   %d files, %s\n", dstCount, formatBytes(dstSize))

	var onlyInSrc, onlyInDst []string
	for f := range srcFiles {
		if !dstFiles[f] {
			onlyInSrc = append(onlyInSrc, f)
		}
	}
	for f := range dstFiles {
		if !srcFiles[f] {
			onlyInDst = append(onlyInDst, f)
		}
	}
	sort.Strings(onlyInSrc)
	sort.Strings(onlyInDst)

	fmt.Println()
	if len(onlyInSrc) == 0 && len(onlyInDst) == 0 {
		fmt.Println("No differences.")
		return nil
	}
	if len(onlyInSrc) > 0 {
		fmt.Printf("Only in source (%d):\n", len(onlyInSrc))
		for _, f := range onlyInSrc {
			fmt.Printf("  %s\n", f)
		}
	}
	if len(onlyInDst) > 0 {
		fmt.Printf("Only in dest (%d):\n", len(onlyInDst))
		for _, f := range onlyInDst {
			fmt.Printf("  %s\n", f)
		}
	}
	return nil
}

func dirStats(root string) (count int64, size int64, files map[string]bool, err error) {
	files = make(map[string]bool)
	var dotCount int64
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dotCount++
			if dotCount%10 == 0 {
				fmt.Print(".")
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), "._") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		count++
		size += info.Size()
		files[rel] = true
		return nil
	})
	return
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func rsyncMirrorArgs(dryRun bool, src, dest string) []string {
	out := []string{"-a", "--no-p", "--delete", "--progress", "--exclude=._*", "--filter=protect ._*"}
	if dryRun {
		out = append([]string{"-v", "-n", "-i"}, out...)
	}
	out = append(out, withTrailingSep(src), withTrailingSep(dest))
	return out
}

func rsyncVersionLine(rsyncPath string) (string, error) {
	out, err := exec.Command(rsyncPath, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("rsync --version: %w", err)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	line = strings.TrimSuffix(strings.TrimSpace(line), "\r")
	return line, nil
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if err != nil {
		if errors.Is(err, io.EOF) {
			return line, nil
		}
		return "", err
	}
	return line, nil
}

func immediateSubdirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read source root: %w", err)
	}
	var out []string
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		if st.IsDir() {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

func parseSelection(line string, numDirs int) []int {
	line = strings.TrimSpace(line)
	if strings.EqualFold(line, "all") {
		out := make([]int, numDirs)
		for i := 0; i < numDirs; i++ {
			out[i] = i
		}
		return out
	}

	line = strings.ReplaceAll(line, ",", " ")
	fields := strings.Fields(line)
	var chosen []int
	for _, tok := range fields {
		n, err := strconv.Atoi(tok)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: ignoring non-integer token: %s\n", tok)
			continue
		}
		idx := n - 1
		if idx < 0 || idx >= numDirs {
			fmt.Fprintf(os.Stderr, "Warning: ignoring out-of-range number: %d\n", n)
			continue
		}
		chosen = append(chosen, idx)
	}
	return chosen
}

func dedupeIndices(in []int) []int {
	seen := make(map[int]struct{}, len(in))
	out := make([]int, 0, len(in))
	for _, idx := range in {
		if _, ok := seen[idx]; ok {
			continue
		}
		seen[idx] = struct{}{}
		out = append(out, idx)
	}
	return out
}

func withTrailingSep(p string) string {
	if p == "" {
		return string(filepath.Separator)
	}
	if strings.HasSuffix(p, string(filepath.Separator)) {
		return p
	}
	return p + string(filepath.Separator)
}
