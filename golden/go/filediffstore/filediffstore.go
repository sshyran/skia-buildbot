package filediffstore

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/boltdb/bolt"
	"github.com/hashicorp/golang-lru"
	"github.com/skia-dev/glog"
	"go.skia.org/infra/go/fileutil"
	"go.skia.org/infra/go/gs"
	"go.skia.org/infra/go/httputils"
	"go.skia.org/infra/go/metrics2"
	"go.skia.org/infra/go/util"
	"go.skia.org/infra/golden/go/diff"
	storage "google.golang.org/api/storage/v1"
)

const (
	DEFAULT_IMG_DIR_NAME         = "images"
	DEFAULT_DIFF_DIR_NAME        = "diffs"
	DEFAULT_DIFFMETRICS_DIR_NAME = "diffmetrics"
	DEFAULT_GS_IMG_DIR_NAME      = "dm-images-v1"
	DEFAULT_TEMPFILE_DIR_NAME    = "__temp"
	DEFAULT_STATUS_DIR_NAME      = "status"
	FAILUREDB_NAME               = "failures.db"
	FAILURE_BUCKET               = "failures"
	IMG_EXTENSION                = "png"
	DIFF_EXTENSION               = "png"
	DIFFMETRICS_EXTENSION        = "json"
	RECOMMENDED_WORKER_POOL_SIZE = 2000
	IMAGE_LRU_CACHE_SIZE         = 500
	METRIC_LRU_CACHE_SIZE        = 100000

	// Limit the number of times diffstore tries to get a file before giving up.
	MAX_URI_GET_TRIES = 4
)

// Interface that the cacheFactory argument must implement.
type CacheFactory func(uniqueId string, codec util.LRUCodec) util.LRUCache

// MemCacheFactory is a cache factory implementation for an in-memory cache.
var MemCacheFactory CacheFactory = func(uniqueId string, code util.LRUCodec) util.LRUCache {
	return util.NewMemLRUCache(0)
}

// DiffMetricsCodec implements the util.LRUCodec to convert between instances
// of diff.DiffMetrics and byte arrays.
// TODO(stephana): Move this to the util package that generates a codec based
// on a instance value.
type DiffMetricsCodec int

func (d DiffMetricsCodec) Encode(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

func (d DiffMetricsCodec) Decode(data []byte) (interface{}, error) {
	var v diff.DiffMetrics
	err := json.Unmarshal(data, &v)
	return &v, err
}

type FileDiffStore struct {
	// The client used to connect to Google Storage.
	client *http.Client

	// The local directory where image digests should be written to.
	localImgDir string

	// The local directory where images diffs should be stored in.
	localDiffDir string

	// The local directory where DiffMetrics should be serialized in.
	localDiffMetricsDir string

	// The local directory where temporary files are written.
	localTempFileDir string

	// Cache for recently used diffmetrics, eviction based on LFU.
	diffCache util.LRUCache

	// LRU cache for images.
	imageCache util.LRUCache

	// The GS bucket where images are stored.
	gsBucketName string

	// The complete GS URL where images are stored.
	storageBaseDir string

	// The channels workers pick up tasks from.
	absPathCh chan *WorkerReq
	getCh     chan *WorkerReq

	// unavailableDigests contains the digests that could not be processed.
	unavailableDigests map[string]*diff.DigestFailure

	// unavailableChan is a channel to add to unavailableDigests.
	unavailableChan chan *diff.DigestFailure

	// unavailableMutex protects unavailableDigests
	unavailableMutex sync.Mutex

	// Mutexes for ensuring safe access to the different local caches.
	diffDirLock   sync.Mutex
	digestDirLock sync.Mutex

	// failureDB stores the digests that have failed to load.
	failureDB *bolt.DB

	// Contains the number of times digests were successfully downloaded from
	// Google Storage.
	downloadSuccessCount *metrics2.Counter
	// Contains the number of times digests failed to download from
	// Google Storage.
	downloadFailureCount *metrics2.Counter
}

// NewFileDiffStore intializes and returns a file based implementation of
// DiffStore. The optional http.Client is used to make HTTP requests to Google
// Storage. If nil is supplied then a default client is used. The baseDir is
// the local base directory where the DEFAULT_IMG_DIR_NAME,
// DEFAULT_DIFF_DIR_NAME and the DEFAULT_DIFFMETRICS_DIR_NAME directories
// exist. gsBucketName is the bucket images will be downloaded from.
// storageBaseDir is the directory in the bucket (if empty
// DEFAULT_GS_IMG_DIR_NAME is used).  workerPoolSize is the max number of
// simultaneous goroutines that will be created when running Get or AbsPath.
// Use RECOMMENDED_WORKER_POOL_SIZE if unsure what this value should be.
func NewFileDiffStore(client *http.Client, baseDir, gsBucketName string, storageBaseDir string, cacheFactory CacheFactory, workerPoolSize int) (diff.DiffStore, error) {
	if client == nil {
		client = httputils.NewTimeoutClient()
	}

	if storageBaseDir == "" {
		storageBaseDir = DEFAULT_GS_IMG_DIR_NAME
	}

	imageCache, err := lru.New(IMAGE_LRU_CACHE_SIZE)
	if err != nil {
		return nil, fmt.Errorf("Unable to alloace image LRU cache: %s", err)
	}

	diffCache := cacheFactory("di", DiffMetricsCodec(0))
	unavailableChan := make(chan *diff.DigestFailure, 10)

	statusDir := fileutil.Must(fileutil.EnsureDirExists(filepath.Join(baseDir, DEFAULT_STATUS_DIR_NAME)))
	failureDB, err := bolt.Open(filepath.Join(statusDir, FAILUREDB_NAME), 0600, nil)
	if err != nil {
		return nil, fmt.Errorf("Unable to open failuredb: %s", err)
	}

	fs := &FileDiffStore{
		client:               client,
		localImgDir:          fileutil.Must(fileutil.EnsureDirExists(filepath.Join(baseDir, DEFAULT_IMG_DIR_NAME))),
		localDiffDir:         fileutil.Must(fileutil.EnsureDirExists(filepath.Join(baseDir, DEFAULT_DIFF_DIR_NAME))),
		localDiffMetricsDir:  fileutil.Must(fileutil.EnsureDirExists(filepath.Join(baseDir, DEFAULT_DIFFMETRICS_DIR_NAME))),
		localTempFileDir:     fileutil.Must(fileutil.EnsureDirExists(filepath.Join(baseDir, DEFAULT_TEMPFILE_DIR_NAME))),
		gsBucketName:         gsBucketName,
		storageBaseDir:       storageBaseDir,
		imageCache:           imageCache,
		diffCache:            diffCache,
		unavailableDigests:   map[string]*diff.DigestFailure{},
		unavailableChan:      unavailableChan,
		failureDB:            failureDB,
		downloadSuccessCount: metrics2.GetCounter("gold.gsdownload", map[string]string{"result": "success"}),
		downloadFailureCount: metrics2.GetCounter("gold.gsdownload", map[string]string{"result": "failure"}),
	}

	if err := fs.loadDigestFailures(); err != nil {
		return nil, err
	}
	go func() {
		for {
			digestFailure := <-unavailableChan
			if err := fs.addDigestFailure(digestFailure); err != nil {
				glog.Errorf("Unable to store digest failure: %s", err)
			} else if err = fs.loadDigestFailures(); err != nil {
				glog.Errorf("Unable to load failures: %s", err)
			}
		}
	}()

	fs.activateWorkers(workerPoolSize)
	return fs, nil
}

// addDigestFailure adds a digest failure to the database.
func (f *FileDiffStore) addDigestFailure(failure *diff.DigestFailure) error {
	jsonData, err := json.Marshal(failure)
	if err != nil {
		return err
	}

	return f.failureDB.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(FAILURE_BUCKET))
		if err != nil {
			return err
		}
		return bucket.Put([]byte(failure.Digest), jsonData)
	})
}

func (f *FileDiffStore) purgeDigestFailures(digests []string) error {
	updated := false
	err := f.failureDB.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(FAILURE_BUCKET))
		if bucket == nil {
			return nil
		}

		for _, d := range digests {
			if bucket.Get([]byte(d)) != nil {
				updated = true
				if err := bucket.Delete([]byte(d)); err != nil {
					return err
				}
			}
		}

		return nil
	})

	if (err == nil) && updated {
		return f.loadDigestFailures()
	}
	return err
}

// loadDigestFailures loads all digest failures to
func (f *FileDiffStore) loadDigestFailures() error {
	newFailures := make(map[string]*diff.DigestFailure, len(f.unavailableDigests))
	err := f.failureDB.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(FAILURE_BUCKET))
		if bucket == nil {
			return nil
		}

		cursor := bucket.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			dFailure := &diff.DigestFailure{}
			if err := json.Unmarshal(v, dFailure); err != nil {
				return err
			}
			newFailures[string(k)] = dFailure
		}
		return nil
	})
	if err == nil {
		f.unavailableMutex.Lock()
		f.unavailableDigests = newFailures
		f.unavailableMutex.Unlock()
	}
	return err
}

func (f *FileDiffStore) PurgeDigests(digests []string, purgeGS bool) error {
	// Remove from GS if requested.
	if purgeGS {
		for _, d := range digests {
			if err := f.removeImageFromGS(d); err != nil {
				return err
			}
		}
	}

	for _, d := range digests {
		if err := f.removeImageFromCache(d); err != nil {
			return err
		}
	}

	// Remove from image cache.
	for _, d := range digests {
		f.imageCache.Remove(d)
	}

	// Remove all metrics from disk cache.
	if err := f.removeDiffMetricsFromFileCache(digests); err != nil {
		return err
	}

	// Remove all diff metrics from LRU cache.
	for _, ki := range f.diffCache.Keys() {
		k := ki.(string)
		for _, d := range digests {
			if strings.Contains(k, d) {
				f.diffCache.Remove(ki)
			}
		}
	}
	return f.purgeDigestFailures(digests)
}

type WorkerReq struct {
	id     interface{}
	respCh chan<- *WorkerResp
}

type WorkerResp struct {
	id  interface{}
	val interface{}
}

type GetId struct {
	dMain  string
	dOther string
}

func (fs *FileDiffStore) activateWorkers(workerPoolSize int) {
	fs.absPathCh = make(chan *WorkerReq, workerPoolSize)
	fs.getCh = make(chan *WorkerReq, workerPoolSize)

	for i := 0; i < workerPoolSize; i++ {
		go func() {
			for {
				select {
				case req := <-fs.absPathCh:
					req.respCh <- &WorkerResp{id: req.id, val: fs.absPathOne(req.id.(string))}
				case req := <-fs.getCh:
					gid := req.id.(GetId)
					req.respCh <- &WorkerResp{id: req.id, val: fs.getOne(gid.dMain, gid.dOther)}
				}
			}
		}()
	}
}

// getOne uses the following algorithm:
// 1. Look for the DiffMetrics of the digests in the local cache.
// If found:
//     2. Return the DiffMetrics.
// Else:
//     3. Make sure the digests exist in the local cache. Download it from
//        Google Storage if necessary.
//     4. Calculate DiffMetrics.
//     5. Write DiffMetrics to the cache and return.
func (fs *FileDiffStore) getOne(dMain, dOther string) interface{} {
	var diffMetrics *diff.DiffMetrics = nil
	var err error

	// 1. Check if the DiffMetrics exists in the memory cache.
	baseName := getDiffBasename(dMain, dOther)
	if obj, ok := fs.diffCache.Get(baseName); ok {
		diffMetrics = obj.(*diff.DiffMetrics)
	} else {
		// Check if it's in the file cache.
		diffMetrics, err = fs.getDiffMetricsFromFileCache(baseName)
		if err != nil {
			glog.Errorf("Failed to getDiffMetricsFromFileCache for digest %s and digest %s: %s", dMain, dOther, err)
			return nil
		}

		if diffMetrics != nil {
			// 2. The DiffMetrics exists locally return it.
			fs.diffCache.Add(baseName, diffMetrics)
		} else {
			// 3. Make sure the digests exist in the local cache. Download it from
			//    Google Storage if necessary.
			if err = fs.ensureDigestInCache(dOther); err != nil {
				glog.Errorf("Failed to ensureDigestInCache for digest %s: %s", dOther, err)
				return nil
			}

			// 4. Calculate DiffMetrics.
			diffMetrics, err = fs.diff(dMain, dOther)
			if err != nil {
				glog.Errorf("Failed to calculate DiffMetrics for digest %s and digest %s: %s", dMain, dOther, err)
				return nil
			}

			// 5. Write DiffMetrics to the local caches.
			fs.diffCache.Add(baseName, diffMetrics)

			// Write to disk in the background.
			writeCopy := *diffMetrics
			go func() {
				if err := fs.writeDiffMetricsToFileCache(baseName, &writeCopy); err != nil {
					glog.Errorf("Failed to write diff metrics to cache for digest %s and digest %s: %s", dMain, dOther, err)
				}
			}()
		}
	}

	// Expand the path of the diff images.
	diffMetrics.PixelDiffFilePath = filepath.Join(fs.localDiffDir, diffMetrics.PixelDiffFilePath)
	return diffMetrics
}

// absPathOne uses the following algorithm:
// 1. Make sure the digests exist in the local cache. Download it from
//    Google Storage if necessary.
// 2. Find and return the absolute path to the digest.
func (fs *FileDiffStore) absPathOne(digest string) interface{} {
	// 1. Make sure we have a local copy of the digest and
	//    download it if necessary. Note: Downloading should
	//    be the exception.
	if err := fs.ensureDigestInCache(digest); err != nil {
		glog.Errorf("Failed to ensureDigestInCache for digest %s: %s", digest, err)
		return nil
	}
	// 2. Find and return the absolute path to the digest.
	return fs.getDigestImageLogicalPath(digest)
}

// Get documentation is found in the diff.DiffStore interface.
// This implementation of Get uses the following algorithm:
// 1. Look for the main digest in local cache else download from Google
//    Storage.
// 2. Create map of digests to their DiffMetrics. This map will be
//    populated and returned.
// 3. Create the channel where responses from workers will be received in.
// 4. Send requests to the request channel.
// The workers will then call getOne with the requests.
// 5. Return map of digests to DiffMetrics once all requests have been
//     processed by the workers.
func (fs *FileDiffStore) Get(dMain string, dRest []string) (map[string]*diff.DiffMetrics, error) {
	if dMain == "" {
		return nil, fmt.Errorf("Received empty dMain digest.")
	}

	// 1. Look for main digest in local cache else download from GS.
	if err := fs.ensureDigestInCache(dMain); err != nil {
		// We cannot compute any DiffMetrics without the main digest.
		// Therefore, fail immediately if the main digest cannot be
		// retrieved.
		return nil, fmt.Errorf("Failed to ensureDigestInCache for main digest %s: %s", dMain, err)
	}

	// 2. Create map of digests to their DiffMetrics. This map will be
	//    populated and returned.
	digestsToDiffMetrics := make(map[string]*diff.DiffMetrics, len(dRest))

	// If the input is empty then we are done. We are doing this here, because
	// if the call to ensureDigestInCache fails we likely have a programming
	// error and we want to catch it.
	if len(dRest) == 0 {
		return digestsToDiffMetrics, nil
	}

	// 3. Create the channel where responses from workers will be received in.
	respCh := make(chan *WorkerResp, len(dRest))

	// 4. Send requests to the request channel.
	digestErrors := 0
	for _, dOther := range dRest {
		if dOther != "" {
			fs.getCh <- &WorkerReq{id: GetId{dMain: dMain, dOther: dOther}, respCh: respCh}
		} else {
			digestErrors++
		}
	}
	for {
		select {
		case resp := <-respCh:
			gid := resp.id.(GetId)
			if val, ok := resp.val.(*diff.DiffMetrics); ok {
				digestsToDiffMetrics[gid.dOther] = val
			} else {
				// This block will be reached when the DiffMetrics could
				// not be calculated due to failures.
				digestErrors++
			}
			if (len(digestsToDiffMetrics) + digestErrors) == len(dRest) {
				// 5. Return map of digests to paths once all requests have
				//    been processed by the workers.
				return digestsToDiffMetrics, nil
			}
		}
	}
}

// AbsPath documentation is found in the diff.DiffStore interface.
// This implementation of AbsPath uses the following algorithm:
// 1. Create map of digests to paths map. This map will be populated and
//    returned.
// 2. Create the channel where responses from workers will be received in.
// 3. Send requests to the request channel.
// The workers will then call absPathOne with the requests.
// 4. Return map of digests to paths once all requests have been processed
//    by the workers.
func (fs *FileDiffStore) AbsPath(digests []string) map[string]string {
	// If the input is empty then we are done.
	if len(digests) == 0 {
		return map[string]string{}
	}

	//  Create map of digests to their paths. This map will be populated
	//   and returned.
	digestsToPaths := make(map[string]string, len(digests))

	// 2. Create the channel where responses from workers will be received
	//    in.
	respCh := make(chan *WorkerResp, len(digests))
	uniques := make(map[string]bool, len(digests))

	// 3. Send requests to the request channel.
	for _, digest := range digests {
		if !uniques[digest] {
			fs.absPathCh <- &WorkerReq{id: digest, respCh: respCh}
			uniques[digest] = true
		}
	}
	digestErrors := 0
	for {
		select {
		case resp := <-respCh:
			digest, _ := resp.id.(string)
			if val, ok := resp.val.(string); ok {
				digestsToPaths[digest] = val
			} else {
				// This block will be reached when the path could not be
				// calculated due to failures.
				digestErrors++
			}
			if (len(digestsToPaths) + digestErrors) == len(digests) {
				// 4. Return map of digests to paths once all requests have
				//    been processed.
				return digestsToPaths
			}
		}
	}
}

// UnavailableDigests is part of the diff.DiffStore interface. See details there.
func (fs *FileDiffStore) UnavailableDigests() map[string]*diff.DigestFailure {
	fs.unavailableMutex.Lock()
	defer fs.unavailableMutex.Unlock()
	return fs.unavailableDigests
}

// TODO(stephana): SetDigestSets is here to satisfy the requirement for the
// DiffStore interface. To be implemented in the next verion of the
// FileDiffStore.
func (fs *FileDiffStore) SetDigestSets(namedDigestSets map[string]map[string]bool) {}

func openDiffMetrics(filepath string) (*diff.DiffMetrics, error) {
	f, err := ioutil.ReadFile(filepath)
	if err != nil {
		return nil, fmt.Errorf("Failed to open DiffMetrics %s for reading: %s", filepath, err)
	}
	diffMetrics := &diff.DiffMetrics{}
	if err := json.Unmarshal(f, diffMetrics); err != nil {
		return nil, fmt.Errorf("Failed to decode diffmetrics: %s", err)
	}
	return diffMetrics, nil
}

func (fs *FileDiffStore) writeDiffMetricsToFileCache(baseName string, diffMetrics *diff.DiffMetrics) error {
	// Lock the mutex before writing to the local diff directory.
	fs.diffDirLock.Lock()
	defer fs.diffDirLock.Unlock()

	// Make paths relative. This has to be reversed in getDiffMetricsFromFileCache.
	fName, err := fs.createDiffMetricPath(baseName)
	if err != nil {
		return err
	}

	f, err := os.Create(fName)
	if err != nil {
		return fmt.Errorf("Unable to create file %s: %s", fName, err)
	}
	defer util.Close(f)

	d, err := json.MarshalIndent(diffMetrics, "", "    ")
	if err != nil {
		return fmt.Errorf("Failed to encode to JSON: %s", err)
	}
	if _, err := f.Write(d); err != nil {
		return fmt.Errorf("Failed to write to file: %v", err)
	}
	return nil
}

func (fs *FileDiffStore) removeDiffMetricsFromFileCache(digests []string) error {
	fs.diffDirLock.Lock()
	defer fs.diffDirLock.Unlock()

	// Walk the entire cache and remove all files are contained in the list of digests.
	return filepath.Walk(fs.localDiffMetricsDir, func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			for _, d := range digests {
				if (len(d) > 0) && strings.Contains(info.Name(), d) {
					if err := os.Remove(path); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

// Returns the file basename to use for the specified digests.
// Eg: Returns 111-222 since 111 < 222 when 111 and 222 are specified as inputs
// regardless of the order.
func getDiffBasename(d1, d2 string) string {
	if d1 < d2 {
		return fmt.Sprintf("%s-%s", d1, d2)
	}
	return fmt.Sprintf("%s-%s", d2, d1)
}

// This method looks for and returns DiffMetrics of the specified digests from the
// local diffmetrics dir. It is thread safe because it locks the diff store's
// mutex before accessing the digest cache.
func (fs *FileDiffStore) getDiffMetricsFromFileCache(baseName string) (*diff.DiffMetrics, error) {
	diffMetricsFilePath := fs.getDiffMetricPath(baseName)

	// Lock the mutex before reading from the local diff directory.
	fs.diffDirLock.Lock()
	defer fs.diffDirLock.Unlock()
	if _, err := os.Stat(diffMetricsFilePath); err != nil {
		if os.IsNotExist(err) {
			// File does not exist.
			return nil, nil
		} else {
			// There was some other error.
			glog.Warningf("Some other error: %s: %s", baseName, err)
			return nil, err
		}
	}

	diffMetrics, err := openDiffMetrics(diffMetricsFilePath)
	if err != nil {
		glog.Warning("Some error opening: %s: %s", baseName, err)
		return nil, err
	}
	return diffMetrics, nil
}

// ensureDigestInCache checks if the image corresponding to digest is cached
// localy. If not it will download it from GS.
func (fs *FileDiffStore) ensureDigestInCache(d string) error {
	exists, err := fs.isDigestInCache(d)
	if err != nil {
		return err
	}
	if !exists {
		// Digest does not exist locally, get it from Google Storage.
		if err := fs.cacheImageFromGS(d); err != nil {
			fs.unavailableChan <- &diff.DigestFailure{
				Digest: d,
				Reason: diff.HTTP,
				TS:     time.Now().Unix(),
				Error:  err.Error(),
			}

			return err
		}
	}
	return nil
}

// This method looks for the specified digest from the local image dir. It is
// thread safe because it locks the diff store's mutext before accessing the digest
// cache.
func (fs *FileDiffStore) isDigestInCache(d string) (bool, error) {
	digestFilePath := fs.getDigestImagePath(d)
	// Lock the mutex before reading from the local digest directory.
	fs.digestDirLock.Lock()
	defer fs.digestDirLock.Unlock()
	if _, err := os.Stat(digestFilePath); err != nil {
		if os.IsNotExist(err) {
			// File does not exist.
			return false, nil
		} else {
			// There was some other error.
			return false, err
		}
	}
	return true, nil
}

// Downloads image file from Google Storage and caches it in a local directory. It
// is thread safe because it locks the diff store's mutext before accessing the
// digest cache. If the provided digest does not exist in Google Storage then
// downloadFailureCount is incremented.
//
func (fs *FileDiffStore) cacheImageFromGS(d string) error {
	storage, err := storage.New(fs.client)
	if err != nil {
		return fmt.Errorf("Failed to create interface to Google Storage: %s\n", err)
	}

	objLocation := filepath.Join(fs.storageBaseDir, fmt.Sprintf("%s.%s", d, IMG_EXTENSION))
	res, err := storage.Objects.Get(fs.gsBucketName, objLocation).Do()
	if err != nil {
		fs.downloadFailureCount.Inc(1)
		return fmt.Errorf("Unable to retrieve: %s/%s:  %s", fs.gsBucketName, objLocation, err)
	}

	for i := 0; i < MAX_URI_GET_TRIES; i++ {
		if i > 0 {
			glog.Warningf("%d. retry for digest %s", i, d)
		}

		err = func() error {
			respBody, err := fs.getRespBody(res)
			if err != nil {
				return err
			}
			defer util.Close(respBody)

			// TODO(stephana): Creating and renaming temporary files this way
			// should be made into a generic utility function.
			// See also FileTileStore for a similar implementation.
			// Create a temporary file.
			tempOut, err := ioutil.TempFile(fs.localTempFileDir, fmt.Sprintf("tempfile-%s", d))
			if err != nil {
				return fmt.Errorf("Unable to create temp file: %s", err)
			}

			md5Hash := md5.New()
			multiOut := io.MultiWriter(md5Hash, tempOut)

			if _, err = io.Copy(multiOut, respBody); err != nil {
				return err
			}
			err = tempOut.Close()
			if err != nil {
				return fmt.Errorf("Error closing temp file: %s", err)
			}

			// Check the MD5.
			objMD5, err := base64.StdEncoding.DecodeString(res.Md5Hash)
			if err != nil {
				return fmt.Errorf("Unable to decode MD5 hash from %s", d)
			}

			if !bytes.Equal(md5Hash.Sum(nil), objMD5) {
				return fmt.Errorf("MD5 hash for digest %s incorrect.", d)
			}

			// Rename the file after we acquired a lock
			outputBaseName := fs.getImageBaseName(d)
			outputFile, err := fs.createRadixPath(fs.localImgDir, outputBaseName)
			if err != nil {
				return fmt.Errorf("Error creating output file: %s", err)
			}

			fs.digestDirLock.Lock()
			defer fs.digestDirLock.Unlock()
			if err := os.Rename(tempOut.Name(), outputFile); err != nil {
				return fmt.Errorf("Unable to move file: %s", err)
			}

			fs.downloadSuccessCount.Inc(1)
			return nil
		}()

		if err == nil {
			break
		}
		glog.Errorf("Error fetching file for digest %s: %s", d, err)
	}

	if err != nil {
		glog.Errorf("Failed fetching file after %d attempts", MAX_URI_GET_TRIES)
		fs.downloadFailureCount.Inc(1)
	}
	return err
}

func (fs *FileDiffStore) removeImageFromGS(d string) error {
	storage, err := storage.New(fs.client)
	if err != nil {
		return fmt.Errorf("Failed to create interface to Google Storage: %s\n", err)
	}

	objLocation := filepath.Join(fs.storageBaseDir, fmt.Sprintf("%s.%s", d, IMG_EXTENSION))
	if err := storage.Objects.Delete(fs.gsBucketName, objLocation).Do(); err != nil {
		return fmt.Errorf("Unable to delete %s/%s:  %s", fs.gsBucketName, objLocation, err)
	}
	return nil
}

func (fs *FileDiffStore) removeImageFromCache(d string) error {
	fs.digestDirLock.Lock()
	defer fs.digestDirLock.Unlock()
	path := fs.getDigestImagePath(d)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	return os.Remove(path)
}

// Returns the response body of the specified GS object. Tries MAX_URI_GET_TRIES
// times if download is unsuccessful. Client must close the response body when
// finished with it.
func (fs *FileDiffStore) getRespBody(res *storage.Object) (io.ReadCloser, error) {
	request, err := gs.RequestForStorageURL(res.MediaLink)
	if err != nil {
		return nil, fmt.Errorf("Unable to create Storage MediaURI request: %s\n", err)
	}

	resp, err := fs.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("Unable to retrieve Storage MediaURI: %s", err)
	}
	if resp.StatusCode != 200 {
		defer util.Close(resp.Body)
		return nil, fmt.Errorf("Failed to retrieve: %d  %s", resp.StatusCode, resp.Status)
	}
	return resp.Body, nil
}

// Calculate the DiffMetrics for the provided digests.
func (fs *FileDiffStore) diff(d1, d2 string) (*diff.DiffMetrics, error) {
	img1, err := fs.getDigestImage(d1)
	if err != nil {
		return nil, err
	}

	img2, err := fs.getDigestImage(d2)
	if err != nil {
		return nil, err
	}
	dm, resultImg := diff.Diff(img1, img2)

	baseName := getDiffBasename(d1, d2)

	// Write the diff image to a temporary file.
	tempOut, err := ioutil.TempFile(fs.localTempFileDir, fmt.Sprintf("tempfile-%s", baseName))
	if err != nil {
		return nil, fmt.Errorf("Unable to create temp file: %s", err)
	}

	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(tempOut, resultImg); err != nil {
		return nil, err
	}

	err = tempOut.Close()
	if err != nil {
		return nil, fmt.Errorf("Error closing temp file: %s", err)
	}

	diffImageFilename := fmt.Sprintf("%s.%s", baseName, IMG_EXTENSION)
	outputFileName, err := fs.createRadixPath(fs.localDiffDir, diffImageFilename)
	if err != nil {
		return nil, err
	}

	fs.diffDirLock.Lock()
	defer fs.diffDirLock.Unlock()
	if err := os.Rename(tempOut.Name(), outputFileName); err != nil {
		return nil, fmt.Errorf("Unable to move file: %s", err)
	}

	// This sets a logical path for this file.
	dm.PixelDiffFilePath = diffImageFilename
	return dm, nil
}

// getDigestImage returns the image corresponding to the digest either from
// RAM or disk.
func (fs *FileDiffStore) getDigestImage(d string) (image.Image, error) {
	var err error
	var img image.Image
	if obj, ok := fs.imageCache.Get(d); ok {
		return obj.(image.Image), nil
	}
	// TODO Should be changed to a safe write that writes to a tmp file then renames it.
	img, err = diff.OpenImage(fs.getDigestImagePath(d))
	if err == nil {
		fs.imageCache.Add(d, img)
		return img, nil
	}

	// Mark the image as unavailable since we were not able to decode it.
	fs.unavailableChan <- &diff.DigestFailure{
		Digest: d,
		Reason: diff.CORRUPTED,
		TS:     time.Now().Unix(),
		Error:  err.Error(),
	}

	return nil, fmt.Errorf("Unable to read image for %s: %s", d, err)
}

// getDigestPath returns the filepath where the image corresponding to the
// give digests should be stored.
func (fs *FileDiffStore) getDigestImagePath(digest string) string {
	return fileutil.TwoLevelRadixPath(fs.localImgDir, fmt.Sprintf("%s.%s", digest, IMG_EXTENSION))
}

// getDigestImageLogicalPath returns the images in a flat directory. As expected
// by the front-end.
func (fs *FileDiffStore) getDigestImageLogicalPath(digest string) string {
	return filepath.Join(fs.localImgDir, fmt.Sprintf("%s.%s", digest, IMG_EXTENSION))
}

// getDiffMetricPath returns the filename where the diffmetric should be
// cached.
func (fs *FileDiffStore) getDiffMetricPath(baseName string) string {
	return fileutil.TwoLevelRadixPath(fs.localDiffMetricsDir, fmt.Sprintf("%s.%s", baseName, DIFFMETRICS_EXTENSION))
}

func (fs *FileDiffStore) createDiffMetricPath(baseName string) (string, error) {
	return fs.createRadixPath(fs.localDiffMetricsDir, fmt.Sprintf("%s.%s", baseName, DIFFMETRICS_EXTENSION))
}

func (fs *FileDiffStore) getImageBaseName(digest string) string {
	return fmt.Sprintf("%s.%s", digest, IMG_EXTENSION)
}

func (fs *FileDiffStore) createRadixPath(baseDir, fileName string) (string, error) {
	targetPath := fileutil.TwoLevelRadixPath(baseDir, fileName)
	radixDir, _ := filepath.Split(targetPath)
	if err := os.MkdirAll(radixDir, 0700); err != nil {
		return "", err
	}

	return targetPath, nil
}
