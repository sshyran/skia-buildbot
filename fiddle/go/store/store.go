// Stores and retrieves fiddles and associated assets in Google Storage.
package store

import (
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/skia-dev/glog"
	"go.skia.org/infra/fiddle/go/types"
	"go.skia.org/infra/go/auth"
	"go.skia.org/infra/go/util"
	"golang.org/x/net/context"
	"google.golang.org/cloud"
	"google.golang.org/cloud/storage"
)

const (
	FIDDLE_STORAGE_BUCKET = "skia-fiddle"
)

// Media is the type of outputs we can get from running a fiddle.
type Media string

// Media constants.
const (
	CPU Media = "CPU"
	GPU Media = "GPU"
	PDF Media = "PDF"
	SKP Media = "SKP"
)

// props records the name and content-type for each type of Media and is used in mediaProps.
type props struct {
	filename    string
	contentType string
}

var (
	mediaProps = map[Media]props{
		CPU: props{filename: "cpu.png", contentType: "image/png"},
		GPU: props{filename: "gpu.png", contentType: "image/png"},
		PDF: props{filename: "pdf.pdf", contentType: "application/pdf"},
		SKP: props{filename: "skp.skp", contentType: "application/octet-stream"},
	}
)

// Store is used to read and write user code and media to and from Google
// Storage.
type Store struct {
	bucket *storage.BucketHandle
}

// New create a new Store.
func New() (*Store, error) {
	// TODO(jcgregorio) Decide is this needs to be a backoff client. May not be necessary if we add caching at this layer.
	client, err := auth.NewDefaultJWTServiceAccountClient(auth.SCOPE_READ_WRITE)
	if err != nil {
		return nil, fmt.Errorf("Problem setting up client OAuth: %s", err)
	}
	storageClient, err := storage.NewClient(context.Background(), cloud.WithBaseHTTP(client))
	if err != nil {
		return nil, fmt.Errorf("Problem creating storage client: %s", err)
	}
	return &Store{
		bucket: storageClient.Bucket(FIDDLE_STORAGE_BUCKET),
	}, nil
}

// writeMediaFile writes a file to Google Storage.
//
//    media - The type of the file to write.
//    fiddleHash - The hash of the fiddle.
//    runId - A unique identifier for the specific run (git checkout of Skia).
//    b64 - The contents of the media file base64 encoded.
func (s *Store) writeMediaFile(media Media, fiddleHash, runId, b64 string) error {
	if b64 == "" {
		return fmt.Errorf("An empty file is not a valid %s file.", string(media))
	}
	p := mediaProps[media]
	if p.filename == "" {
		return fmt.Errorf("Unknown media type.")
	}
	path := strings.Join([]string{"fiddle", fiddleHash, runId, p.filename}, "/")
	w := s.bucket.Object(path).NewWriter(context.Background())
	defer util.Close(w)
	w.ObjectAttrs.ContentEncoding = p.contentType
	body, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("Media wasn't properly encoded base64: %s", err)
	}
	if n, err := w.Write(body); err != nil {
		return fmt.Errorf("There was a problem storing the media for %s. Uploaded %d bytes: %s", string(media), n, err)
	}
	return nil
}

// Put writes the code and media to Google Storage.
//
//    code - The user's code.
//    options - The options the user chose to run the code under.
//    gitHash - The git checkout this was built under.
//    ts - The timestamp of the gitHash.
//    results - The results from running fiddle_run.
//
// Code is written to:
//
//   gs://skia-fiddle/fiddle/<fiddleHash>/draw.cpp
//
// And media files are written to:
//
//   gs://skia-fiddle/fiddle/<fiddleHash>/<runId>/cpu.png
//   gs://skia-fiddle/fiddle/<fiddleHash>/<runId>/gpu.png
//   gs://skia-fiddle/fiddle/<fiddleHash>/<runId>/skp.skp
//   gs://skia-fiddle/fiddle/<fiddleHash>/<runId>/pdf.pdf
//
// Where runId is <git commit timestamp in RFC3339>:<git commit hash>.
//
// If results is nil then only the code is written.
//
// Returns the fiddleHash.
func (s *Store) Put(code string, options types.Options, gitHash string, ts time.Time, results *types.Result) (string, error) {
	fiddleHash, err := options.ComputeHash(code)
	if err != nil {
		return "", fmt.Errorf("Could not compute hash for the code: %s", err)
	}
	// Write code.
	path := strings.Join([]string{"fiddle", fiddleHash, "draw.cpp"}, "/")
	w := s.bucket.Object(path).NewWriter(context.Background())
	defer util.Close(w)
	w.ObjectAttrs.ContentEncoding = "text/plain"
	w.ObjectAttrs.Metadata = map[string]string{
		"width":  fmt.Sprintf("%d", options.Width),
		"height": fmt.Sprintf("%d", options.Height),
		"source": fmt.Sprintf("%d", options.Source),
	}
	if n, err := w.Write([]byte(code)); err != nil {
		return "", fmt.Errorf("There was a problem storing the code. Uploaded %d bytes: %s", n, err)
	}
	// Write media, if any.
	if results == nil {
		return fiddleHash, nil
	}
	if err := s.PutMedia(fiddleHash, gitHash, ts, results); err != nil {
		return fiddleHash, err
	}
	return fiddleHash, nil
}

// PutMedia writes the media for the given fiddleHash to Google Storage.
//
//    fiddleHash - The fiddle hash.
//    gitHash - The git checkout this was built under.
//    ts - The timestamp of the gitHash.
//    results - The results from running fiddle_run.
//
// Media files are written to:
//
//   gs://skia-fiddle/fiddle/<fiddleHash>/<runId>/cpu.png
//   gs://skia-fiddle/fiddle/<fiddleHash>/<runId>/gpu.png
//   gs://skia-fiddle/fiddle/<fiddleHash>/<runId>/skp.skp
//   gs://skia-fiddle/fiddle/<fiddleHash>/<runId>/pdf.pdf
//
// Where runId is <git commit timestamp in RFC3339>:<git commit hash>.
//
// If results is nil then only the code is written.
//
// Returns the fiddleHash.
func (s *Store) PutMedia(fiddleHash string, gitHash string, ts time.Time, results *types.Result) error {
	// Write each of the media files.
	runId := fmt.Sprintf("%s:%s", ts.UTC().Format(time.RFC3339), gitHash)
	err := s.writeMediaFile(CPU, fiddleHash, runId, results.Execute.Output.Raster)
	if err != nil {
		return err
	}
	err = s.writeMediaFile(GPU, fiddleHash, runId, results.Execute.Output.Gpu)
	if err != nil {
		return err
	}
	err = s.writeMediaFile(PDF, fiddleHash, runId, results.Execute.Output.Pdf)
	if err != nil {
		return err
	}
	err = s.writeMediaFile(SKP, fiddleHash, runId, results.Execute.Output.Skp)
	if err != nil {
		return err
	}
	return nil
}

// GetCode returns the code and options for the given fiddle hash.
//
//    fiddleHash - The fiddle hash.
//
// Returns the code and the options the code was run under.
func (s *Store) GetCode(fiddleHash string) (string, *types.Options, error) {
	o := s.bucket.Object(fmt.Sprintf("fiddle/%s/draw.cpp", fiddleHash))
	r, err := o.NewReader(context.Background())
	if err != nil {
		return "", nil, fmt.Errorf("Failed to open source file for %s: %s", fiddleHash, err)
	}
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return "", nil, fmt.Errorf("Failed to read source file for %s: %s", fiddleHash, err)
	}
	attr, err := o.Attrs(context.Background())
	if err != nil {
		return "", nil, fmt.Errorf("Failed to read attributes for %s: %s", fiddleHash, err)
	}
	width, err := strconv.Atoi(attr.Metadata["width"])
	if err != nil {
		return "", nil, fmt.Errorf("Failed to parse options width: %s", err)
	}
	height, err := strconv.Atoi(attr.Metadata["height"])
	if err != nil {
		return "", nil, fmt.Errorf("Failed to parse options height: %s", err)
	}
	source, err := strconv.Atoi(attr.Metadata["source"])
	if err != nil {
		return "", nil, fmt.Errorf("Failed to parse options source: %s", err)
	}
	options := &types.Options{
		Width:  width,
		Height: height,
		Source: source,
	}
	return string(b), options, nil
}

// GetMedia returns the file, content-type, filename, and error for a given fiddle hash and type of media.
//
//    media - The type of the file to write.
//    fiddleHash - The hash of the fiddle.
//
// Returns the media file contents as a byte slice, the content-type, and the filename of the media.
func (s *Store) GetMedia(fiddleHash string, media Media) ([]byte, string, string, error) {
	// List the dirs under gs://skia-fiddle/fiddle/<fiddleHash>/ and find the most recent one.
	// Use Delimiter and Prefix to get a directory listing of sub-directories. See
	// https://cloud.google.com/storage/docs/json_api/v1/objects/list
	q := &storage.Query{
		Delimiter: "/",
		Prefix:    fmt.Sprintf("fiddle/%s/", fiddleHash),
	}
	runIds := []string{}
	ctx := context.Background()
	for {
		list, err := s.bucket.List(ctx, q)
		if err != nil {
			return nil, "", "", fmt.Errorf("Failed to retrieve list of results for (%s, %s): %s", fiddleHash, string(media), err)
		}
		for _, name := range list.Prefixes {
			runIds = append(runIds, name)
		}
		if list.Next == nil {
			break
		}
		q = list.Next
	}
	if len(runIds) == 0 {
		return nil, "", "", fmt.Errorf("This fiddle has no valid output written (%s, %s)", fiddleHash, string(media))
	}
	sort.Strings(runIds)
	r, err := s.bucket.Object(runIds[0] + mediaProps[media].filename).NewReader(ctx)
	if err != nil {
		return nil, "", "", fmt.Errorf("Unable to get reader for the media file (%s, %s): %s", fiddleHash, string(media), err)
	}
	defer util.Close(r)
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, "", "", fmt.Errorf("Unable to read the media file (%s, %s): %s", fiddleHash, string(media), err)
	}
	return b, mediaProps[media].contentType, mediaProps[media].filename, nil
}

// AddSource adds a new source image. The image must be a PNG.
//
//    image - The bytes on the PNG file.
//
// Returns the id of the source image.
func AddSource(image []byte) (int, error) {
	// Use the file 'lastid.txt' in the bucket that contains the last id used.
	// Read, record gen, increments, write with condition of unchanged generation.

	// TODO(jcgregorio) Implement.
	return 0, fmt.Errorf("Not implemented yet.")
}

// downloadSingleSourceImage downloads a single source image from the Google Storage bucket.
//
//    ctx - The context of the request.
//    bucket - The Google Storage bucket.
//    srcName - The full Google Storage path of the source image.
//    dstName - The full local file system name where the source image will be written to.
func downloadSingleSourceImage(ctx context.Context, bucket *storage.BucketHandle, srcName, dstName string) error {
	r, err := bucket.Object(srcName).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("Failed to open reader for image %s: %s", srcName, err)
	}
	defer util.Close(r)
	w, err := os.Create(dstName)
	if err != nil {
		return fmt.Errorf("Failed to open writer for image %s: %s", dstName, err)
	}
	defer util.Close(w)
	_, err = io.Copy(w, r)
	if err != nil {
		return fmt.Errorf("Failed to copy bytes for image %s: %s", dstName, err)
	}
	return nil
}

// DownloadAllSourceImages downloads all the images under gs://skia-fiddles/source/
// and copies them as PNG images under FIDDLE_ROOT/images/.
//
//    fiddleRoot - The root directory where fiddle is working. See DESIGN.md.
func (s *Store) DownloadAllSourceImages(fiddleRoot string) error {
	ctx := context.Background()
	q := &storage.Query{
		Prefix: fmt.Sprintf("source/"),
	}
	if err := os.MkdirAll(filepath.Join(fiddleRoot, "images"), 0755); err != nil {
		return fmt.Errorf("Failed to create images directory: %s", err)
	}
	for {
		list, err := s.bucket.List(ctx, q)
		if err != nil {
			return fmt.Errorf("Failed to retrieve image list: %s", err)
		}
		for _, res := range list.Results {
			filename := strings.Split(res.Name, "/")[1]
			dstFullPath := filepath.Join(fiddleRoot, "images", filename)
			if err := downloadSingleSourceImage(ctx, s.bucket, res.Name, dstFullPath); err != nil {
				glog.Errorf("Failed to download image %q: %s", res.Name, err)
			}
		}
		if list.Next == nil {
			break
		}
		q = list.Next
	}
	return nil
}