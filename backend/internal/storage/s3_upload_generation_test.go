package storage

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestS3GenerationKeyLayout(t *testing.T) {
	// No configured prefix
	g := newS3UploadGeneration(&s3Storage{bucket: "bucket", prefix: ""}, "tok")
	require.Equal(t, "logo.txt", g.liveKey("logo.txt"))
	require.Equal(t, ImportStagingSegment+"/tok/logo.txt", g.stagedKey("logo.txt"))
	require.Equal(t, ImportBackupSegment+"/tok/logo.txt", g.backupKey("logo.txt"))
	require.Equal(t, ImportStagingSegment+"/tok/manifest", g.manifestKey())

	// A configured root prefix namespaces everything
	g2 := newS3UploadGeneration(&s3Storage{bucket: "bucket", prefix: "data/uploads"}, "tok")
	require.Equal(t, "data/uploads/logo.txt", g2.liveKey("logo.txt"))
	require.Equal(t, "data/uploads/"+ImportStagingSegment+"/tok/logo.txt", g2.stagedKey("logo.txt"))
	require.Equal(t, "data/uploads/"+ImportBackupSegment+"/tok/logo.txt", g2.backupKey("logo.txt"))

	// Backup/staging keys stay inside the reserved namespaces regardless of sub-paths
	for _, k := range []string{g2.stagedKey("a/b/c"), g2.backupKey("a/b/c")} {
		require.True(t, strings.Contains(k, ImportStagingSegment+"/") || strings.Contains(k, ImportBackupSegment+"/"))
	}
}

func TestS3CopySourceEscaping(t *testing.T) {
	g := newS3UploadGeneration(&s3Storage{bucket: "bucket", prefix: ""}, "tok")
	require.Equal(t, "bucket/data/uploads/logo.txt", g.copySource("data/uploads/logo.txt"))
	require.Equal(t, "bucket/a%20b/c=d.txt", g.copySource("a b/c=d.txt"))
}

func TestS3ReservedPathsHiddenFromListing(t *testing.T) {
	paths := []string{
		"application-images/logo.svg",
		ImportStagingSegment + "/tok/logo.txt",
		ImportBackupSegment + "/tok/logo.txt",
	}
	for _, p := range paths[1:] {
		require.True(t, IsReservedImportPath(p))
	}
	require.False(t, IsReservedImportPath(paths[0]))
}
