package taskflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/Gere2/neurofs/internal/audit"
	"github.com/Gere2/neurofs/internal/fsutil"
	"github.com/Gere2/neurofs/internal/models"
	"github.com/Gere2/neurofs/internal/runid"
)

// PersistRunBundle materializes the mutable task-cache bundle as an immutable,
// run-scoped artifact. Uncorrelated commands keep using the cache path and
// report available=false; they cannot form a trustworthy JoinKey.
func PersistRunBundle(
	ctx context.Context,
	repoRoot string,
	bundle models.Bundle,
) (key runid.JoinKey, absolutePath string, available bool, retErr error) {
	attribution, err := runid.Current(ctx)
	if err != nil {
		return key, "", false, err
	}
	if !attribution.Available() {
		return key, "", false, nil
	}
	if bundle.BundleHash == "" || bundle.BundleHash != audit.BundleHash(bundle) {
		return key, "", false, fmt.Errorf("bundle hash is missing or does not match canonical content")
	}

	rel := path.Join(
		".neurofs", "runs", attribution.RunID.String(), "bundles",
		bundle.BundleHash+".bundle.json",
	)
	key = runid.JoinKey{
		RunID:      attribution.RunID,
		BundlePath: rel,
		BundleHash: bundle.BundleHash,
	}
	if err := key.Validate(); err != nil {
		return runid.JoinKey{}, "", false, err
	}
	absolutePath, err = fsutil.ConfineToRepo(repoRoot, filepath.FromSlash(rel))
	if err != nil {
		return runid.JoinKey{}, "", false, fmt.Errorf("resolve run bundle path: %w", err)
	}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return runid.JoinKey{}, "", false, fmt.Errorf("marshal run bundle: %w", err)
	}
	if err := writeImmutableBundle(absolutePath, data, key.BundleHash); err != nil {
		return runid.JoinKey{}, "", false, err
	}
	return key, absolutePath, true, nil
}

func writeImmutableBundle(filePath string, data []byte, expectedBundleHash string) (retErr error) {
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		return fmt.Errorf("create run bundle directory: %w", err)
	}
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return verifyPersistedRunBundle(filePath, expectedBundleHash)
	}
	if err != nil {
		return fmt.Errorf("create immutable run bundle: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close immutable run bundle: %w", err))
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write immutable run bundle: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync immutable run bundle: %w", err)
	}
	return nil
}

func verifyPersistedRunBundle(filePath, expectedBundleHash string) error {
	data, _, err := fsutil.ReadRegularFileBounded(filePath, maxTaskArtifactBytes)
	if err != nil {
		return fmt.Errorf("read existing immutable run bundle: %w", err)
	}
	var bundle models.Bundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return fmt.Errorf("decode existing immutable run bundle: %w", err)
	}
	if bundle.BundleHash != expectedBundleHash || audit.BundleHash(bundle) != expectedBundleHash {
		return fmt.Errorf("immutable run bundle already exists with different content: %s", filePath)
	}
	return nil
}

// LoadRunBundle consumes a complete JoinKey. It confines and re-reads the
// exact file, then verifies both its declared and recomputed canonical hash.
func LoadRunBundle(repoRoot string, key runid.JoinKey) (models.Bundle, error) {
	if err := key.Validate(); err != nil {
		return models.Bundle{}, err
	}
	abs, err := fsutil.ConfineToRepoStrict(repoRoot, filepath.FromSlash(key.BundlePath))
	if err != nil {
		return models.Bundle{}, fmt.Errorf("resolve joined bundle: %w", err)
	}
	data, _, err := fsutil.ReadRegularFileBounded(abs, maxTaskArtifactBytes)
	if err != nil {
		return models.Bundle{}, fmt.Errorf("read joined bundle: %w", err)
	}
	var bundle models.Bundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return models.Bundle{}, fmt.Errorf("decode joined bundle: %w", err)
	}
	if bundle.BundleHash != key.BundleHash || audit.BundleHash(bundle) != key.BundleHash {
		return models.Bundle{}, fmt.Errorf("joined bundle hash mismatch for %s", key.BundlePath)
	}
	return bundle, nil
}
