package protondrive

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DefaultMaxArtifactBytes is the per-file size limit for a copy (1 GB).
const DefaultMaxArtifactBytes int64 = 1 << 30

// RunFolderLayout is the name of each run's folder in Proton Drive.
// Only folders with exactly this shape are ever deleted by Ballast.
const RunFolderLayout = "2006-01-02_150405"

// runFolderRE matches the folders Ballast itself creates.
var runFolderRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_\d{6}$`)

// File is one local file to copy.
type File struct {
	// Name is the name it gets in Proton Drive (a plain file name).
	Name string
	// Path is where it is on this machine.
	Path string
}

// Request is one service run's copy.
type Request struct {
	// Service names the service; its files go under <folder>/<Service>.
	Service string
	// RunTime names the run folder (UTC).
	RunTime time.Time
	// Folder is the Proton Drive folder holding everything Ballast copies.
	Folder string
	// Files are the verified artifacts to send.
	Files []File
	// SkippedFolders names parts that are folders of live files, which
	// are never sent here (only archive and dump files are); they are
	// listed in the result.
	SkippedFolders []string
	// MaxBytes is the size limit per file; zero is DefaultMaxArtifactBytes.
	MaxBytes int64
	// KeepLast is how many run folders to keep; zero is 7.
	KeepLast int
	// WorkDir is a scratch folder (staging and the read-back download).
	WorkDir string
}

// Result is how a copy went. A copy is Verified only when every file
// was uploaded, seen at the right size, and one was downloaded and
// matched its original.
type Result struct {
	// Verified is true for a copy that was read back successfully.
	Verified bool
	// SignedOut is true when the login is gone and the user must sign in.
	SignedOut bool
	// Detail is a plain-English summary.
	Detail string
	// Uploaded and Skipped list file names.
	Uploaded []string
	Skipped  []string
	// ReadBack is the file that was downloaded and compared.
	ReadBack string
}

// SignInAgainText is the phrase a copy that failed for lack of a login
// always contains, so a screen can offer to sign in again.
const SignInAgainText = "sign in again"

// Copy uploads req.Files to Proton Drive, verifies the copy, and
// applies keep-last. It never returns an error: every problem is a
// Result that is not Verified, because this tier only reports (spec
// §12) and must never fail a job.
func (c *Client) Copy(ctx context.Context, req Request) Result {
	res := c.copy(ctx, req)
	if errors.Is(res.err, ErrSignedOut) {
		res.Result.SignedOut = true
		res.Result.Detail = "Proton Drive needs you to " + SignInAgainText + ". Open the Destinations page and choose \"Sign in to Proton Drive again\". Nothing was copied this time."
	} else if res.err != nil {
		res.Result.Detail = fmt.Sprintf("The Proton Drive copy could not be completed: %v", res.err)
	}
	return res.Result
}

type copyOutcome struct {
	Result
	err error
}

func (c *Client) copy(ctx context.Context, req Request) copyOutcome {
	var out copyOutcome
	max := req.MaxBytes
	if max <= 0 {
		max = DefaultMaxArtifactBytes
	}
	keep := req.KeepLast
	if keep <= 0 {
		keep = 7
	}
	seen := map[string]bool{}
	var send []File
	var sizes = map[string]int64{}
	for _, f := range req.Files {
		if f.Name == "" || strings.ContainsAny(f.Name, `/\`) {
			out.err = fmt.Errorf("internal error: %q is not a plain file name", f.Name)
			return out
		}
		if seen[f.Name] {
			out.err = fmt.Errorf("two files would both be called %s", f.Name)
			return out
		}
		seen[f.Name] = true
		st, err := os.Stat(f.Path)
		if err != nil {
			out.err = fmt.Errorf("reading %s: %w", f.Name, err)
			return out
		}
		if st.Size() > max {
			out.Skipped = append(out.Skipped, fmt.Sprintf("%s (%s is over the %s limit)", f.Name, humanBytes(st.Size()), humanBytes(max)))
			continue
		}
		sizes[f.Name] = st.Size()
		send = append(send, f)
	}
	for _, name := range req.SkippedFolders {
		out.Skipped = append(out.Skipped, fmt.Sprintf("%s (a folder of files; only archive and dump files are copied to Proton Drive)", name))
	}
	if len(send) == 0 {
		out.Detail = "Nothing was copied to Proton Drive: " + skippedText(out.Skipped, "there were no files to send")
		return out
	}

	release, err := c.Lock(ctx)
	if err != nil {
		out.err = err
		return out
	}
	defer release()

	stage, err := os.MkdirTemp(req.WorkDir, "proton-stage-")
	if err != nil {
		out.err = fmt.Errorf("preparing files to upload: %w", err)
		return out
	}
	defer os.RemoveAll(stage)
	runName := req.RunTime.UTC().Format(RunFolderLayout)
	runDir := filepath.Join(stage, req.Service, runName)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		out.err = fmt.Errorf("preparing files to upload: %w", err)
		return out
	}
	for _, f := range send {
		if err := linkOrCopy(f.Path, filepath.Join(runDir, f.Name)); err != nil {
			out.err = fmt.Errorf("preparing %s to upload: %w", f.Name, err)
			return out
		}
	}

	folder := strings.TrimRight(req.Folder, "/")
	if _, err := c.exec(ctx, "filesystem", "upload", "-f", "skip", "-d", "merge", filepath.Join(stage, req.Service), folder); err != nil {
		out.err = fmt.Errorf("uploading to %s: %w", folder, err)
		return out
	}
	remoteRun := path.Join(folder, req.Service, runName)
	for _, f := range send {
		e, err := c.Info(ctx, path.Join(remoteRun, f.Name))
		if err != nil {
			out.err = fmt.Errorf("checking %s after upload: %w", f.Name, err)
			return out
		}
		if e.Size != sizes[f.Name] {
			out.Detail = fmt.Sprintf("Proton Drive holds %s at %s but the original is %s, so the copy cannot be trusted.", f.Name, humanBytes(e.Size), humanBytes(sizes[f.Name]))
			return out
		}
		out.Uploaded = append(out.Uploaded, f.Name)
	}

	// Read one file back in full, a different one on different runs.
	pick := send[int(req.RunTime.Unix()/60)%len(send)]
	got, err := c.downloadSum(ctx, path.Join(remoteRun, pick.Name), pick.Name, req.WorkDir)
	if err != nil {
		out.err = fmt.Errorf("downloading %s to check it: %w", pick.Name, err)
		return out
	}
	want, err := fileSum(pick.Path)
	if err != nil {
		out.err = fmt.Errorf("reading %s: %w", pick.Name, err)
		return out
	}
	out.ReadBack = pick.Name
	if got != want {
		out.Detail = fmt.Sprintf("The copy of %s in Proton Drive does not match the original (checksums differ), so the copy cannot be trusted.", pick.Name)
		return out
	}

	out.Verified = true
	out.Detail = fmt.Sprintf("Copied %d file(s) to Proton Drive and checked them; downloaded %s and it matched.", len(send), pick.Name)
	if len(out.Skipped) > 0 {
		out.Detail += " Not copied: " + strings.Join(out.Skipped, "; ") + "."
	}
	if note := c.prune(ctx, path.Join(folder, req.Service), keep); note != "" {
		out.Detail += " " + note
	}
	return out
}

func skippedText(skipped []string, none string) string {
	if len(skipped) == 0 {
		return none
	}
	return strings.Join(skipped, "; ") + "."
}

// prune trashes and then deletes this service's run folders beyond the
// newest keep. Only folders named like Ballast's own run folders are
// touched; anything else in the folder is left alone. A problem is
// returned as a note, never as a failure of the copy.
func (c *Client) prune(ctx context.Context, serviceFolder string, keep int) string {
	entries, err := c.List(ctx, serviceFolder)
	if err != nil {
		return fmt.Sprintf("Old copies could not be cleaned up: %v.", err)
	}
	var runs []string
	for _, e := range entries {
		if runFolderRE.MatchString(e.Name) && (e.Type == "" || e.IsFolder()) {
			runs = append(runs, e.Name)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(runs)))
	if len(runs) <= keep {
		return ""
	}
	var failed []string
	for _, name := range runs[keep:] {
		p := path.Join(serviceFolder, name)
		if _, err := c.exec(ctx, "filesystem", "trash", p); err != nil {
			failed = append(failed, name)
			continue
		}
		if _, err := c.exec(ctx, "filesystem", "delete", p); err != nil {
			failed = append(failed, name)
		}
	}
	if len(failed) > 0 {
		return fmt.Sprintf("Old copies could not be removed: %s.", strings.Join(failed, ", "))
	}
	return ""
}

func (c *Client) downloadSum(ctx context.Context, remote, name, work string) (string, error) {
	dir, err := os.MkdirTemp(work, "proton-readback-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	if _, err := c.exec(ctx, "filesystem", "download", remote, dir); err != nil {
		return "", err
	}
	return fileSum(filepath.Join(dir, name))
}

func fileSum(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// linkOrCopy makes dst hold src's bytes, by hard link when the folders
// share a disk (no extra space) and by copy otherwise.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
