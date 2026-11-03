package storage

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"uuid"
)

// filesystemUploadGeneration stages a new generation in a hidden directory next to the upload root
//
// Both directories live in the parent of the upload root on purpose: the switch is a sequence of os.Rename calls, which are only atomic when source and destination are on the same filesystem, and the staging tree must be invisible to storage Walk/List calls.
type filesystemUploadGeneration struct {
	rootAbs string
	workDir string
	newDir  string
	oldDir  string

	// manifest lists every relative path staged for this generation, including files already switched into live storage
	manifest string
}

func newFilesystemUploadGeneration(s *filesystemStorage, token string) (*filesystemUploadGeneration, error) {
	rootAbs := s.absoluteRootPath
	workDir := filesystemStagingWorkDir(rootAbs, token)
	g := &filesystemUploadGeneration{
		rootAbs:  rootAbs,
		workDir:  workDir,
		newDir:   filepath.Join(workDir, "new"),
		oldDir:   filepath.Join(workDir, "old"),
		manifest: filepath.Join(workDir, "manifest"),
	}
	if err := os.MkdirAll(g.newDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create import staging directory %q: %w", g.newDir, err)
	}
	return g, nil
}

// safeJoin joins relPath under root and guarantees the result stays within root
func safeJoin(root, relPath string) (string, error) {
	relPath = filepath.Clean(filepath.FromSlash(relPath))
	if relPath == "." || filepath.IsAbs(relPath) || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) || relPath == ".." {
		return "", fmt.Errorf("invalid upload path %q", relPath)
	}
	return filepath.Join(root, relPath), nil
}

func (g *filesystemUploadGeneration) StageFile(_ context.Context, relPath string, data io.Reader) error {
	target, err := safeJoin(g.newDir, relPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return fmt.Errorf("failed to create directories for %q: %w", relPath, err)
	}

	// Write to a temporary file and rename into the staging tree, so an interrupted write never leaves a partial staged file
	tmp := target + "." + uuid.NewV4().String() + "-tmp"
	tmpFile, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("failed to open %q for writing: %w", relPath, err)
	}
	if _, err := io.Copy(tmpFile, data); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("failed to write staged file %q: %w", relPath, err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("failed to close staged file %q: %w", relPath, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("failed to finalize staged file %q: %w", relPath, err)
	}

	// Append the path to the manifest before considering the file staged
	// The manifest is the only way a resumed rollback can learn about files that were already switched live and no longer exist in the staging tree
	return g.appendManifest(relPath)
}

func (g *filesystemUploadGeneration) appendManifest(relPath string) error {
	f, err := os.OpenFile(g.manifest, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to open import manifest: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(filepath.ToSlash(relPath) + "\n"); err != nil {
		return fmt.Errorf("failed to write import manifest: %w", err)
	}
	return nil
}

func (g *filesystemUploadGeneration) readManifest() ([]string, error) {
	f, err := os.Open(g.manifest)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to open import manifest: %w", err)
	}
	defer f.Close()

	var paths []string
	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		p := strings.TrimSpace(scanner.Text())
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	return paths, scanner.Err()
}

func (g *filesystemUploadGeneration) Commit(_ context.Context) error {
	// Fall back to walking the staging tree when no manifest was written yet, which is the first run before the switch starts
	paths, err := g.manifestPaths()
	if err != nil {
		return err
	}

	for _, rel := range paths {
		newPath, err := safeJoin(g.newDir, rel)
		if err != nil {
			return err
		}
		if _, err := os.Stat(newPath); err != nil {
			if os.IsNotExist(err) {
				// Already switched live in a previous run
				continue
			}
			return fmt.Errorf("failed to stat staged file %q: %w", rel, err)
		}

		livePath, err := safeJoin(g.rootAbs, rel)
		if err != nil {
			return err
		}
		if _, err := os.Stat(livePath); err == nil {
			// The live file is the previous generation and must be moved aside before the new one takes its place
			oldPath, err := safeJoin(g.oldDir, rel)
			if err != nil {
				return err
			}
			if _, err := os.Stat(oldPath); os.IsNotExist(err) {
				if err := os.MkdirAll(filepath.Dir(oldPath), 0700); err != nil {
					return fmt.Errorf("failed to create backup directories for %q: %w", rel, err)
				}
				if err := os.Rename(livePath, oldPath); err != nil {
					return fmt.Errorf("failed to back up file %q: %w", rel, err)
				}
			} else if err != nil {
				return fmt.Errorf("failed to stat backup file %q: %w", rel, err)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("failed to stat live file %q: %w", rel, err)
		}

		if err := os.MkdirAll(filepath.Dir(livePath), 0700); err != nil {
			return fmt.Errorf("failed to create live directories for %q: %w", rel, err)
		}
		if err := os.Rename(newPath, livePath); err != nil {
			return fmt.Errorf("failed to switch file %q live: %w", rel, err)
		}
	}
	return nil
}

func (g *filesystemUploadGeneration) Rollback(_ context.Context) error {
	paths, err := g.manifestPaths()
	if err != nil {
		return err
	}

	for _, rel := range paths {
		newPath, err := safeJoin(g.newDir, rel)
		if err != nil {
			return err
		}
		if _, statErr := os.Stat(newPath); statErr == nil {
			// The staged file is still in the staging tree, so the switch never touched live storage for this path
			continue
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("failed to stat staged file %q: %w", rel, statErr)
		}

		// The staged file is gone, so it was switched live and must be reversed
		livePath, err := safeJoin(g.rootAbs, rel)
		if err != nil {
			return err
		}
		oldPath, err := safeJoin(g.oldDir, rel)
		if err != nil {
			return err
		}

		if _, err := os.Stat(oldPath); err == nil {
			// The file existed before the import, so its previous content must return live
			if err := os.RemoveAll(livePath); err != nil {
				return fmt.Errorf("failed to remove imported file %q: %w", rel, err)
			}
			if err := os.MkdirAll(filepath.Dir(livePath), 0700); err != nil {
				return fmt.Errorf("failed to create live directories for %q: %w", rel, err)
			}
			if err := os.Rename(oldPath, livePath); err != nil {
				return fmt.Errorf("failed to restore file %q: %w", rel, err)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("failed to stat backup file %q: %w", rel, err)
		} else if err := os.Remove(livePath); err != nil && !os.IsNotExist(err) {
			// The file did not exist before the import and was switched live, so it is removed
			return fmt.Errorf("failed to remove new file %q: %w", rel, err)
		}
	}
	return nil
}

func (g *filesystemUploadGeneration) Cleanup(_ context.Context) error {
	if err := os.RemoveAll(g.workDir); err != nil {
		return fmt.Errorf("failed to remove import work directory %q: %w", g.workDir, err)
	}
	return nil
}

// WorkDir exposes the staging directory so the caller can place other per-generation artifacts (such as the staged archive) next to it
func (g *filesystemUploadGeneration) WorkDir() string {
	return g.workDir
}

// manifestPaths returns every staged relative path, using the manifest when present and falling back to the staging tree
func (g *filesystemUploadGeneration) manifestPaths() ([]string, error) {
	paths, err := g.readManifest()
	if err != nil {
		return nil, err
	}
	if len(paths) > 0 {
		return paths, nil
	}

	// First run, before the manifest exists: every file in the staging tree belongs to the generation
	err = filepath.WalkDir(g.newDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(g.newDir, p)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to walk staging tree: %w", err)
	}
	return paths, nil
}
