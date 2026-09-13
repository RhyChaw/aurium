package driver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// archiveImage is the tiny image used to read and write volume contents.
// Volumes are only reachable from inside a container, so moving their bytes to
// and from the host needs a helper container regardless of driver.
const archiveImage = "alpine:3.20"

// ArchiveVolume streams a volume's contents into a zstd-compressed tarball on
// the host (§6.2 step 4).
//
// The archive is written by the helper container into a bind-mounted directory
// rather than through stdout, so a partial write is visible as a short file
// instead of silently truncating a "successful" snapshot.
func (d *Docker) ArchiveVolume(ctx context.Context, volume, destPath string) error {
	dir := filepath.Dir(destPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	name := filepath.Base(destPath)

	_, err := d.run(ctx, "run", "--rm",
		"-v", volume+":/from:ro",
		"-v", dir+":/to",
		archiveImage,
		"sh", "-c", fmt.Sprintf(
			"apk add --no-cache zstd >/dev/null 2>&1 || true; "+
				"cd /from && tar cf - . | zstd -T0 -q -o /to/%s", shellSafe(name)))
	if err != nil {
		return fmt.Errorf("driver/docker: archive volume %s: %w", volume, err)
	}
	if _, err := os.Stat(destPath); err != nil {
		return fmt.Errorf("driver/docker: archive of %s produced no file: %w", volume, err)
	}
	return nil
}

// RestoreVolume recreates a volume from an archive (§6.3 step 4).
func (d *Docker) RestoreVolume(ctx context.Context, volume, srcPath string) error {
	if _, err := os.Stat(srcPath); err != nil {
		return fmt.Errorf("driver/docker: volume archive missing: %w", err)
	}
	if _, err := d.run(ctx, "volume", "create", volume); err != nil {
		return err
	}

	_, err := d.run(ctx, "run", "--rm",
		"-v", volume+":/to",
		"-v", filepath.Dir(srcPath)+":/from:ro",
		archiveImage,
		"sh", "-c", fmt.Sprintf(
			"apk add --no-cache zstd >/dev/null 2>&1 || true; "+
				// Empty the volume first: restoring must replace, not merge.
				// A merge would leave files the snapshot does not have.
				"rm -rf /to/* /to/.[!.]* 2>/dev/null || true; "+
				"zstd -dc /from/%s | tar xf - -C /to", shellSafe(filepath.Base(srcPath))))
	if err != nil {
		return fmt.Errorf("driver/docker: restore volume %s: %w", volume, err)
	}
	return nil
}

// ArchiveVolume is unsupported on the local driver: there are no volumes,
// only directories the user already owns.
func (l *Local) ArchiveVolume(ctx context.Context, volume, destPath string) error {
	return fmt.Errorf("driver/local: archive volume: %w", ErrUnsupported)
}

func (l *Local) RestoreVolume(ctx context.Context, volume, srcPath string) error {
	return fmt.Errorf("driver/local: restore volume: %w", ErrUnsupported)
}

// shellSafe rejects anything that could break out of the sh -c string. Volume
// and archive names are derived from project and branch names, which come from
// the user, so they are never interpolated unchecked.
func shellSafe(s string) string {
	for _, r := range s {
		ok := r == '.' || r == '-' || r == '_' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return "invalid-name"
		}
	}
	return s
}
