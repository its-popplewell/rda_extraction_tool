package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// stampDir holds a copy of the request and its fingerprint, so status can
// rebuild the plan and a different request cannot mix pieces into this directory.
const stampDir = ".rdaextract"

var errSkipped = errors.New("already done")

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func failLog(out string, p Piece) string {
	base := strings.TrimSuffix(filepath.Base(p.Out), ".nc")
	return filepath.Join(out, ".pieces", "failed", p.Dataset+"_"+base+".log")
}

func writeAtomic(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.tmp%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// stamp records the request in the output directory. A directory holding pieces
// for a different fingerprint is refused, unless force, which moves the old
// pieces aside to .pieces-<old fingerprint>.
func stamp(out, reqPath, fp string, force bool) error {
	dir := filepath.Join(out, stampDir)
	if old, err := os.ReadFile(filepath.Join(dir, "fingerprint")); err == nil {
		prev := strings.TrimSpace(string(old))
		if prev != fp {
			if !force {
				return fmt.Errorf("%s holds pieces for request %s, not %s; use a new -o, or --force to move the old pieces aside",
					out, prev, fp)
			}
			pieces := filepath.Join(out, ".pieces")
			if fileExists(pieces) {
				aside := filepath.Join(out, ".pieces-"+prev)
				if err := os.Rename(pieces, aside); err != nil {
					return err
				}
				fmt.Printf("moved the old pieces to %s\n", aside)
			}
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(reqPath)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "request.yaml"), data); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "fingerprint"), []byte(fp+"\n"))
}

// runPiece runs cdo into <out>.partial and renames it only on success, so a
// final file always means a complete piece. On failure cdo's output is kept in
// <out>/.pieces/failed/.
func runPiece(ctx context.Context, cdo, out string, p Piece) error {
	if fileExists(p.Out) {
		return errSkipped
	}
	if err := os.MkdirAll(filepath.Dir(p.Out), 0o755); err != nil {
		return err
	}
	partial := p.Out + ".partial"
	args := append(slices.Clone(p.Args), p.Source, partial)
	cmd := exec.CommandContext(ctx, cdo, args...)
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log

	logPath := failLog(out, p)
	if err := cmd.Run(); err != nil {
		os.Remove(partial)
		_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
		msg := fmt.Sprintf("%s %s\n\n%s\n%v\n", cdo, strings.Join(args, " "), log.String(), err)
		_ = os.WriteFile(logPath, []byte(msg), 0o644)
		return err
	}
	if err := os.Rename(partial, p.Out); err != nil {
		return err
	}
	_ = os.Remove(logPath) // a retry that succeeded clears the old failure
	return nil
}

type runResult struct {
	Done, Skipped int64
	Failed        []Piece
}

func runPieces(ctx context.Context, cdo, out string, pieces []Piece, workers int) runResult {
	var res runResult
	var mu sync.Mutex
	var finished atomic.Int64
	total := int64(len(pieces))
	start := time.Now()

	jobs := make(chan Piece)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				err := runPiece(ctx, cdo, out, p)
				switch {
				case errors.Is(err, errSkipped):
					atomic.AddInt64(&res.Skipped, 1)
				case err != nil:
					mu.Lock()
					res.Failed = append(res.Failed, p)
					mu.Unlock()
				default:
					atomic.AddInt64(&res.Done, 1)
				}
				if n := finished.Add(1); n%200 == 0 || n == total {
					fmt.Printf("  %d/%d pieces (%s)\n", n, total, time.Since(start).Round(time.Second))
				}
			}
		}()
	}

feed:
	for _, p := range pieces {
		select {
		case jobs <- p:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	return res
}
