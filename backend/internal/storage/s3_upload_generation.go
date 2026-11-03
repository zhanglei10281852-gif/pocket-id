package storage

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// s3UploadGeneration stages new objects under a reserved prefix and switches them object by object
//
// S3 has no atomic directory swap, so the switch is a deterministic, idempotent convergence driven by the staged prefix and a manifest: each live object is copied into the backup namespace before the staged object replaces it, so the same sequence resumes after a crash and the reverse sequence rolls it back.
type s3UploadGeneration struct {
	s3     *s3Storage
	token  string
	client *s3.Client
}

func newS3UploadGeneration(s *s3Storage, token string) *s3UploadGeneration {
	return &s3UploadGeneration{s3: s, token: token, client: s.client}
}

func (g *s3UploadGeneration) liveKey(rel string) string {
	return g.s3.buildObjectKey(rel)
}

func (g *s3UploadGeneration) stagedKey(rel string) string {
	return g.s3.buildObjectKey(ImportStagingSegment + "/" + g.token + "/" + rel)
}

func (g *s3UploadGeneration) backupKey(rel string) string {
	return g.s3.buildObjectKey(ImportBackupSegment + "/" + g.token + "/" + rel)
}

func (g *s3UploadGeneration) manifestKey() string {
	return g.stagedKey("manifest")
}

// copySource builds the CopySource value, URL-escaping every segment except the key separators
func (g *s3UploadGeneration) copySource(key string) string {
	segments := strings.Split(key, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return g.s3.bucket + "/" + strings.Join(segments, "/")
}

func (g *s3UploadGeneration) StageFile(ctx context.Context, relPath string, data io.Reader) error {
	_, err := g.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(g.s3.bucket),
		Key:    aws.String(g.stagedKey(relPath)),
		Body:   data,
	})
	if err != nil {
		return fmt.Errorf("failed to stage file %q: %w", relPath, err)
	}
	return nil
}

func (g *s3UploadGeneration) objectExists(ctx context.Context, key string) (bool, error) {
	_, err := g.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(g.s3.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	if isS3NotFound(err) {
		return false, nil
	}
	return false, err
}

func (g *s3UploadGeneration) copyObject(ctx context.Context, srcKey, dstKey string) error {
	_, err := g.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(g.s3.bucket),
		CopySource: aws.String(g.copySource(srcKey)),
		Key:        aws.String(dstKey),
	})
	if err != nil {
		return fmt.Errorf("failed to copy %q to %q: %w", srcKey, dstKey, err)
	}
	return nil
}

// listRelPaths lists the relative paths staged under the given reserved segment for this generation
func (g *s3UploadGeneration) listRelPaths(ctx context.Context, segment string) ([]string, error) {
	prefix := g.s3.buildObjectKey(segment + "/" + g.token + "/")
	paginator := s3.NewListObjectsV2Paginator(g.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(g.s3.bucket),
		Prefix: aws.String(prefix),
	})

	var paths []string
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list %s objects: %w", segment, err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			if key == prefix || strings.HasSuffix(key, "/") {
				continue
			}
			rel, ok := strings.CutPrefix(key, prefix)
			if !ok || rel == "manifest" {
				continue
			}
			paths = append(paths, rel)
		}
	}
	return paths, nil
}

// ensureManifest returns every staged relative path, recording them in a manifest object the moment the switch begins
//
// Once a staged object is switched live it disappears from the staging prefix, so without the manifest a rollback could no longer tell a newly imported file from a pre-existing one after a mid-switch crash.
func (g *s3UploadGeneration) ensureManifest(ctx context.Context) ([]string, error) {
	body, err := g.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(g.s3.bucket),
		Key:    aws.String(g.manifestKey()),
	})
	if err == nil {
		defer body.Body.Close()
		return parseManifest(body.Body)
	}
	if !isS3NotFound(err) {
		return nil, fmt.Errorf("failed to read import manifest: %w", err)
	}

	// First switch run: the staging prefix still holds every staged object
	paths, err := g.listRelPaths(ctx, ImportStagingSegment)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	for _, p := range paths {
		buf.WriteString(p)
		buf.WriteByte('\n')
	}
	_, err = g.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(g.s3.bucket),
		Key:    aws.String(g.manifestKey()),
		Body:   &buf,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to write import manifest: %w", err)
	}
	return paths, nil
}

func parseManifest(r io.Reader) ([]string, error) {
	var paths []string
	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(r)
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
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to parse import manifest: %w", err)
	}
	return paths, nil
}

func (g *s3UploadGeneration) Commit(ctx context.Context) error {
	paths, err := g.ensureManifest(ctx)
	if err != nil {
		return err
	}

	for _, rel := range paths {
		staged := g.stagedKey(rel)
		stagedExists, err := g.objectExists(ctx, staged)
		if err != nil {
			return fmt.Errorf("failed to check staged file %q: %w", rel, err)
		}
		if !stagedExists {
			// Already switched live in a previous run
			continue
		}

		live := g.liveKey(rel)
		liveExists, err := g.objectExists(ctx, live)
		if err != nil {
			return fmt.Errorf("failed to check live file %q: %w", rel, err)
		}
		if liveExists {
			backup := g.backupKey(rel)
			backupExists, err := g.objectExists(ctx, backup)
			if err != nil {
				return fmt.Errorf("failed to check backup file %q: %w", rel, err)
			}
			if !backupExists {
				if err := g.copyObject(ctx, live, backup); err != nil {
					return err
				}
			}
		}

		if err := g.copyObject(ctx, staged, live); err != nil {
			return err
		}
		if _, err := g.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(g.s3.bucket),
			Key:    aws.String(staged),
		}); err != nil {
			return fmt.Errorf("failed to remove staged file %q: %w", rel, err)
		}
	}
	return nil
}

func (g *s3UploadGeneration) Rollback(ctx context.Context) error {
	// The manifest may be missing if the process died before the switch began, in which case live storage was never touched
	paths, err := g.ensureManifest(ctx)
	if err != nil {
		return err
	}

	for _, rel := range paths {
		// A staged object that is still present means the switch never touched the live key for this path
		stagedExists, err := g.objectExists(ctx, g.stagedKey(rel))
		if err != nil {
			return fmt.Errorf("failed to check staged file %q: %w", rel, err)
		}
		if stagedExists {
			continue
		}

		// The staged object is gone, so the live key holds an imported object and must be reversed
		backup := g.backupKey(rel)
		backupExists, err := g.objectExists(ctx, backup)
		if err != nil {
			return fmt.Errorf("failed to check backup file %q: %w", rel, err)
		}
		live := g.liveKey(rel)
		if backupExists {
			if err := g.copyObject(ctx, backup, live); err != nil {
				return err
			}
			continue
		}

		// The file did not exist before the import and was switched live, so it is removed
		_, delErr := g.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(g.s3.bucket),
			Key:    aws.String(live),
		})
		if delErr != nil && !isS3NotFound(delErr) {
			return fmt.Errorf("failed to remove new live file %q: %w", rel, delErr)
		}
	}
	return nil
}

func (g *s3UploadGeneration) Cleanup(ctx context.Context) error {
	for _, segment := range []string{ImportStagingSegment, ImportBackupSegment} {
		if err := g.deletePrefix(ctx, segment+"/"+g.token+"/"); err != nil {
			return err
		}
	}
	return nil
}

// deletePrefix removes every object under the given reserved segment, batched as S3 requires
func (g *s3UploadGeneration) deletePrefix(ctx context.Context, segmentPath string) error {
	prefix := g.s3.buildObjectKey(segmentPath)
	paginator := s3.NewListObjectsV2Paginator(g.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(g.s3.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("failed to list objects under %q: %w", prefix, err)
		}
		if len(page.Contents) == 0 {
			continue
		}

		objects := make([]s3types.ObjectIdentifier, 0, len(page.Contents))
		for _, obj := range page.Contents {
			objects = append(objects, s3types.ObjectIdentifier{Key: obj.Key})
		}
		_, err = g.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(g.s3.bucket),
			Delete: &s3types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("failed to delete objects under %q: %w", prefix, err)
		}
	}
	return nil
}
