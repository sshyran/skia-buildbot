package common

import (
	"fmt"
	"os"
	"strings"

	"github.com/skia-dev/glog"
	"go.skia.org/infra/fuzzer/go/config"
	"go.skia.org/infra/go/exec"
	"go.skia.org/infra/go/gitinfo"
	storage "google.golang.org/api/storage/v1"
)

// DownloadSkiaVersionForFuzzing downloads the version of Skia specified in Google Storage
// to the given path.  It returns an error on failure.
func DownloadSkiaVersionForFuzzing(storageService *storage.Service, path string) error {
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Could not clean Skia Path %s: %s", path, err)
	}
	if err := os.MkdirAll(path, 0755); err != nil {
		return fmt.Errorf("Could not create Skia Path %s: %s", path, err)
	}

	skiaVersion, err := getSkiaVersionFromGCS(storageService)
	if err != nil {
		return fmt.Errorf("Could not get Skia version from GCS: %s", err)
	}

	return downloadSkia(skiaVersion, path)
}

// getSkiaVersionFromGCS checks the skia_version folder in the fuzzer bucket for a single file
// that has the version to be used for fuzzing (typically a dep roll).  It returns the version
// or an error if there was a failure.
func getSkiaVersionFromGCS(storageService *storage.Service) (string, error) {
	if storageService == nil {
		return "", fmt.Errorf("Storage service cannot be nil!")
	}
	contents, err := storageService.Objects.List(config.GS.Bucket).Prefix("skia_version").Do()
	if err != nil {
		return "", err
	}
	if len(contents.Items) < 2 {
		return "", fmt.Errorf("version file not found")
	}
	// item[0] is the folder skia_version/, the file name of item[1] is the current version to fuzz
	file := contents.Items[1].Name
	return strings.TrimLeft(file, "skia_version/"), nil
}

// downloadSkia uses git to clone Skia from googlesource.com and check it out to the specified version
// It returns an error on failure.
func downloadSkia(version, path string) error {
	glog.Infof("Cloning Skia version %s to %s", version, path)

	repo, err := gitinfo.CloneOrUpdate("https://skia.googlesource.com/skia", path, false)
	if err != nil {
		return fmt.Errorf("Failed cloning Skia: %s", err)
	}

	if err = repo.SetToCommit(version); err != nil {
		return fmt.Errorf("Problem setting Skia to version %s: %s", version, err)
	}

	syncCmd := &exec.Command{
		Name: "bin/sync-and-gyp",
		Dir:  path,
	}

	if err := exec.Run(syncCmd); err != nil {
		return fmt.Errorf("Failed syncing and setting up gyp: %s", err)
	}

	if lc, err := repo.Details(version); err != nil {
		glog.Errorf("Could not get git details for skia version %s: %s", version, err)
	} else {
		config.SetSkiaVersion(lc)
	}

	return nil
}