package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"time"

	"github.com/gorilla/mux"
	"github.com/skia-dev/glog"
	"go.skia.org/infra/go/auth"
	"go.skia.org/infra/go/common"
	"go.skia.org/infra/go/database"
	"go.skia.org/infra/go/eventbus"
	"go.skia.org/infra/go/fileutil"
	"go.skia.org/infra/go/gitinfo"
	"go.skia.org/infra/go/httputils"
	"go.skia.org/infra/go/influxdb"
	"go.skia.org/infra/go/issues"
	"go.skia.org/infra/go/login"
	"go.skia.org/infra/go/metadata"
	"go.skia.org/infra/go/redisutil"
	"go.skia.org/infra/go/rietveld"
	"go.skia.org/infra/go/skiaversion"
	"go.skia.org/infra/go/timer"
	tracedb "go.skia.org/infra/go/trace/db"
	"go.skia.org/infra/go/util"
	"go.skia.org/infra/golden/go/db"
	"go.skia.org/infra/golden/go/digeststore"
	"go.skia.org/infra/golden/go/expstorage"
	"go.skia.org/infra/golden/go/filediffstore"
	"go.skia.org/infra/golden/go/goldingestion"
	"go.skia.org/infra/golden/go/history"
	"go.skia.org/infra/golden/go/ignore"
	"go.skia.org/infra/golden/go/indexer"
	"go.skia.org/infra/golden/go/status"
	"go.skia.org/infra/golden/go/storage"
	"go.skia.org/infra/golden/go/trybot"
	"go.skia.org/infra/golden/go/types"
	gstorage "google.golang.org/api/storage/v1"
)

// Command line flags.
var (
	authWhiteList      = flag.String("auth_whitelist", login.DEFAULT_DOMAIN_WHITELIST, "White space separated list of domains and email addresses that are allowed to login.")
	cpuProfile         = flag.Duration("cpu_profile", 0, "Duration for which to profile the CPU usage. After this duration the program writes the CPU profile and exits.")
	doOauth            = flag.Bool("oauth", true, "Run through the OAuth 2.0 flow on startup, otherwise use a GCE service account.")
	forceLogin         = flag.Bool("force_login", false, "Force the user to be authenticated for all requests.")
	gsBucketName       = flag.String("gs_bucket", "chromium-skia-gm", "Name of the google storage bucket that holds uploaded images.")
	imageDir           = flag.String("image_dir", "/tmp/imagedir", "What directory to store test and diff images in.")
	issueTrackerKey    = flag.String("issue_tracker_key", "", "API Key for accessing the project hosting API.")
	local              = flag.Bool("local", false, "Running locally if true. As opposed to in production.")
	memProfile         = flag.Duration("memprofile", 0, "Duration for which to profile memory. After this duration the program writes the memory profile and exits.")
	nCommits           = flag.Int("n_commits", 50, "Number of recent commits to include in the analysis.")
	nTilesToBackfill   = flag.Int("backfill_tiles", 0, "Number of tiles to backfill in our history of tiles.")
	oauthCacheFile     = flag.String("oauth_cache_file", "/home/perf/google_storage_token.data", "Path to the file where to cache cache the oauth credentials.")
	port               = flag.String("port", ":9000", "HTTP service address (e.g., ':9000')")
	redirectURL        = flag.String("redirect_url", "https://gold.skia.org/oauth2callback/", "OAuth2 redirect url. Only used when local=false.")
	redisDB            = flag.Int("redis_db", 0, "The index of the Redis database we should use. Default will work fine in most cases.")
	redisHost          = flag.String("redis_host", "", "The host and port (e.g. 'localhost:6379') of the Redis data store that will be used for caching.")
	resourcesDir       = flag.String("resources_dir", "", "The directory to find templates, JS, and CSS files. If blank the directory relative to the source code files will be used.")
	rietveldURL        = flag.String("rietveld_url", "https://codereview.chromium.org/", "URL of the Rietveld instance where we retrieve CL metadata.")
	storageDir         = flag.String("storage_dir", "/tmp/gold-storage", "Directory to store reproducible application data.")
	gitRepoDir         = flag.String("git_repo_dir", "../../../skia", "Directory location for the Skia repo.")
	gitRepoURL         = flag.String("git_repo_url", "https://skia.googlesource.com/skia", "The URL to pass to git clone for the source repository.")
	serviceAccountFile = flag.String("service_account_file", "", "Credentials file for service account.")
	traceservice       = flag.String("trace_service", "localhost:10000", "The address of the traceservice endpoint.")

	influxHost     = flag.String("influxdb_host", influxdb.DEFAULT_HOST, "The InfluxDB hostname.")
	influxUser     = flag.String("influxdb_name", influxdb.DEFAULT_USER, "The InfluxDB username.")
	influxPassword = flag.String("influxdb_password", influxdb.DEFAULT_PASSWORD, "The InfluxDB password.")
	influxDatabase = flag.String("influxdb_database", influxdb.DEFAULT_DATABASE, "The InfluxDB database.")
)

const (
	IMAGE_URL_PREFIX = "/img/"

	// OAUTH2_CALLBACK_PATH is callback endpoint used for the Oauth2 flow.
	OAUTH2_CALLBACK_PATH = "/oauth2callback/"
)

// TODO(stephana): Simplify
// the ResponseEnvelope and use it solely to wrap JSON arrays.
// Remove sendResponse and sendErrorResponse in favor of sendJsonResponse
// and httputils.ReportError.

// ResponseEnvelope wraps all responses. Some fields might be empty depending
// on context or whether there was an error or not.
type ResponseEnvelope struct {
	Data       *interface{}                  `json:"data"`
	Err        *string                       `json:"err"`
	Status     int                           `json:"status"`
	Pagination *httputils.ResponsePagination `json:"pagination"`
}

type PathToURLConverter func(string) string

var (
	// Module level variables that need to be accessible to main2.go.
	storages           *storage.Storage
	pathToURLConverter PathToURLConverter
	statusWatcher      *status.StatusWatcher
	ixr                *indexer.Indexer
	issueTracker       issues.IssueTracker
)

// sendResponse wraps the data of a succesful response in a response envelope
// and sends it to the client.
func sendResponse(w http.ResponseWriter, data interface{}, status int, pagination *httputils.ResponsePagination) {
	resp := ResponseEnvelope{&data, nil, status, pagination}
	sendJson(w, &resp, status)
}

// sendJson serializes the response envelope and sends ito the client.
func sendJson(w http.ResponseWriter, resp *ResponseEnvelope, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// parseJson extracts the body from the request and parses it into the
// provided interface.
func parseJson(r *http.Request, v interface{}) error {
	// TODO (stephana): validate the JSON against a schema. Might not be necessary !
	defer util.Close(r.Body)
	decoder := json.NewDecoder(r.Body)
	return decoder.Decode(v)
}

// URLAwareFileServer wraps around a standard file server and allows to generate
// URLs for a given path that is contained in the root.
type URLAwareFileServer struct {
	// baseDir is the root directory for all content served. All paths have to
	// be contained somewhere in the directory tree below this.
	baseDir string

	// baseUrl is the URL prefix that maps to baseDir.
	baseUrl string

	// Handler is a standard FileServer handler.
	Handler http.Handler
}

func NewURLAwareFileServer(baseDir, baseUrl string) *URLAwareFileServer {
	absPath, err := filepath.Abs(baseDir)
	if err != nil {
		glog.Fatalf("Unable to get abs path of %s. Got error: %s", baseDir, err)
	}

	fileHandler := http.StripPrefix(baseUrl, http.FileServer(http.Dir(absPath)))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		// TODO(stephana): Expose a handler in the diff store so we don't have
		// to mangle the URL from the outside.
		r.URL.Path = fileutil.TwoLevelRadixPath(r.URL.Path)

		// Cache images for 12 hours.
		w.Header().Set("Cache-control", "public, max-age=43200")
		fileHandler.ServeHTTP(w, r)
	})

	return &URLAwareFileServer{
		baseDir: absPath,
		baseUrl: baseUrl,
		Handler: handler,
	}
}

// converToUrl returns the path component of a URL given the path
// contained within baseDir.
func (ug *URLAwareFileServer) GetURL(path string) string {
	absPath, err := filepath.Abs(path)
	if err != nil {
		glog.Errorf("Unable to get absolute path of %s. Got error: %s", path, err)
		return ""
	}

	relPath, err := filepath.Rel(ug.baseDir, absPath)
	if err != nil {
		glog.Errorf("Unable to find subpath got error %s", err)
		return ""
	}

	ret := ug.baseUrl + relPath
	return ret
}

func main() {
	defer common.LogPanic()
	var err error

	mainTimer := timer.New("main init")
	// Setup DB flags.
	dbConf := database.ConfigFromFlags(db.PROD_DB_HOST, db.PROD_DB_PORT, database.USER_RW, db.PROD_DB_NAME, db.MigrationSteps())

	// Global init to initialize
	common.InitWithMetrics2("skiacorrectness", influxHost, influxUser, influxPassword, influxDatabase, local)

	v, err := skiaversion.GetVersion()
	if err != nil {
		glog.Fatalf("Unable to retrieve version: %s", err)
	}
	glog.Infof("Version %s, built at %s", v.Commit, v.Date)

	// Enable the memory profiler if memProfile was set.
	// TODO(stephana): This should be moved to a HTTP endpoint that
	// only responds to internal IP addresses/ports.
	if *memProfile > 0 {
		time.AfterFunc(*memProfile, func() {
			glog.Infof("Writing Memory Profile")
			f, err := ioutil.TempFile("./", "memory-profile")
			if err != nil {
				glog.Fatalf("Unable to create memory profile file: %s", err)
			}
			if err := pprof.WriteHeapProfile(f); err != nil {
				glog.Fatalf("Unable to write memory profile file: %v", err)
			}
			util.Close(f)
			glog.Infof("Memory profile written to %s", f.Name())

			os.Exit(0)
		})
	}

	if *cpuProfile > 0 {
		glog.Infof("Writing CPU Profile")
		f, err := ioutil.TempFile("./", "cpu-profile")
		if err != nil {
			glog.Fatalf("Unable to create cpu profile file: %s", err)
		}

		if err := pprof.StartCPUProfile(f); err != nil {
			glog.Fatalf("Unable to write cpu profile file: %v", err)
		}
		time.AfterFunc(*cpuProfile, func() {
			pprof.StopCPUProfile()
			util.Close(f)
			glog.Infof("CPU profile written to %s", f.Name())
			os.Exit(0)
		})
	}

	// Set the resource directory if it's empty. Useful for running locally.
	if *resourcesDir == "" {
		_, filename, _, _ := runtime.Caller(0)
		*resourcesDir = filepath.Join(filepath.Dir(filename), "../..")
		*resourcesDir += "/frontend"
	}

	// Set up login
	// TODO (stephana): Factor out to go/login/login.go and removed hard coded
	// values.
	var cookieSalt = "notverysecret"
	var clientID = "31977622648-ubjke2f3staq6ouas64r31h8f8tcbiqp.apps.googleusercontent.com"
	var clientSecret = "rK-kRY71CXmcg0v9I9KIgWci"
	var useRedirectURL = fmt.Sprintf("http://localhost%s/oauth2callback/", *port)
	if !*local {
		cookieSalt = metadata.Must(metadata.ProjectGet(metadata.COOKIESALT))
		clientID = metadata.Must(metadata.ProjectGet(metadata.CLIENT_ID))
		clientSecret = metadata.Must(metadata.ProjectGet(metadata.CLIENT_SECRET))
		useRedirectURL = *redirectURL
	}
	login.Init(clientID, clientSecret, useRedirectURL, cookieSalt, login.DEFAULT_SCOPE, *authWhiteList, *local)

	// Get the client to be used to access GS and the Monorail issue tracker.
	client, err := auth.NewJWTServiceAccountClient("", *serviceAccountFile, nil, gstorage.CloudPlatformScope, "https://www.googleapis.com/auth/userinfo.email")
	if err != nil {
		glog.Fatalf("Failed to authenticate service account: %s", err)
	}

	// Set up the cache implementation to use.
	cacheFactory := filediffstore.MemCacheFactory
	if *redisHost != "" {
		cacheFactory = func(uniqueId string, codec util.LRUCodec) util.LRUCache {
			// This RedisLRUCache never gets closed, but there is only one that is created and used until the program exits.
			return redisutil.NewRedisLRUCache(*redisHost, *redisDB, uniqueId, codec)
		}
	}

	// Get the expecations storage, the filediff storage and the tilestore.
	diffStore, err := filediffstore.NewFileDiffStore(client, *imageDir, *gsBucketName, filediffstore.DEFAULT_GS_IMG_DIR_NAME, cacheFactory, filediffstore.RECOMMENDED_WORKER_POOL_SIZE)
	if err != nil {
		glog.Fatalf("Allocating DiffStore failed: %s", err)
	}

	if !*local {
		if err := dbConf.GetPasswordFromMetadata(); err != nil {
			glog.Fatal(err)
		}
	}
	vdb, err := dbConf.NewVersionedDB()
	if err != nil {
		glog.Fatal(err)
	}

	if !vdb.IsLatestVersion() {
		glog.Fatal("Wrong DB version. Please updated to latest version.")
	}

	digestStore, err := digeststore.New(*storageDir)
	if err != nil {
		glog.Fatal(err)
	}

	git, err := gitinfo.CloneOrUpdate(*gitRepoURL, *gitRepoDir, false)
	if err != nil {
		glog.Fatal(err)
	}

	evt := eventbus.New(nil)

	rietveldAPI := rietveld.New(rietveld.RIETVELD_SKIA_URL, httputils.NewTimeoutClient())

	// Connect to traceDB and create the builders.
	db, err := tracedb.NewTraceServiceDBFromAddress(*traceservice, types.GoldenTraceBuilder)
	if err != nil {
		glog.Fatalf("Failed to connect to tracedb: %s", err)
	}

	masterTileBuilder, err := tracedb.NewMasterTileBuilder(db, git, *nCommits, evt)
	if err != nil {
		glog.Fatalf("Failed to build trace/db.DB: %s", err)
	}
	branchTileBuilder := tracedb.NewBranchTileBuilder(db, git, rietveldAPI, evt)

	ingestionStore, err := goldingestion.NewIngestionStore(*traceservice)
	if err != nil {
		glog.Fatalf("Unable to open ingestion store: %s", err)
	}
	storages = &storage.Storage{
		DiffStore:         diffStore,
		ExpectationsStore: expstorage.NewCachingExpectationStore(expstorage.NewSQLExpectationStore(vdb), evt),
		MasterTileBuilder: masterTileBuilder,
		BranchTileBuilder: branchTileBuilder,
		DigestStore:       digestStore,
		NCommits:          *nCommits,
		EventBus:          evt,
		TrybotResults:     trybot.NewTrybotResults(branchTileBuilder, rietveldAPI, ingestionStore),
		RietveldAPI:       rietveldAPI,
	}

	// TODO(stephana): Remove this workaround to avoid circular dependencies once the 'storage' module is cleaned up.
	storages.IgnoreStore = ignore.NewSQLIgnoreStore(vdb, storages.ExpectationsStore, storages.GetTileStreamNow(time.Minute))

	if err := history.Init(storages, *nTilesToBackfill); err != nil {
		glog.Fatalf("Unable to initialize history package: %s", err)
	}

	if err := ignore.Init(storages.IgnoreStore); err != nil {
		glog.Fatalf("Failed to start monitoring for expired ignore rules: %s", err)
	}

	// Rebuild the index every two minutes.
	ixr, err = indexer.New(storages, 2*time.Minute)
	if err != nil {
		glog.Fatalf("Failed to create indexer: %s", err)
	}

	if !*local {
		*issueTrackerKey = metadata.Must(metadata.ProjectGet(metadata.APIKEY))
	}

	issueTracker = issues.NewMonorailIssueTracker(client)

	statusWatcher, err = status.New(storages)
	if err != nil {
		glog.Fatalf("Failed to initialize status watcher: %s", err)
	}

	imgFS := NewURLAwareFileServer(*imageDir, IMAGE_URL_PREFIX)
	pathToURLConverter = imgFS.GetURL
	mainTimer.Stop()

	router := mux.NewRouter()

	// Set up the resource to serve the image files.
	router.PathPrefix(IMAGE_URL_PREFIX).Handler(imgFS.Handler)

	// New Polymer based UI endpoints.
	router.PathPrefix("/res/").HandlerFunc(makeResourceHandler(*resourcesDir))

	// TODO(stephana): Remove the 'poly' prefix from all the handlers and clean
	// up main2.go by either merging it it into main.go or making it clearer that
	// it contains all the handlers. Make it clearer what variables are shared
	// between the different file.

	// All the handlers will be prefixed with poly to differentiate it from the
	// angular code until the angular code is removed.
	router.HandleFunc(OAUTH2_CALLBACK_PATH, login.OAuth2CallbackHandler)

	// /_/hashes is used by the bots to find hashes it does not need to upload.
	router.HandleFunc("/_/hashes", textAllHashesHandler).Methods("GET")
	router.HandleFunc("/json/version", skiaversion.JsonHandler)
	router.HandleFunc("/loginstatus/", login.StatusHandler)
	router.HandleFunc("/logout/", login.LogoutHandler)

	// json handlers only used by the new UI.
	router.HandleFunc("/json/byblame", jsonByBlameHandler).Methods("GET")
	router.HandleFunc("/json/list", jsonListTestsHandler).Methods("GET")
	router.HandleFunc("/json/paramset", jsonParamsHandler).Methods("GET")
	router.HandleFunc("/json/search", jsonSearchHandler).Methods("GET")
	router.HandleFunc("/json/diff", jsonDiffHandler).Methods("GET")
	router.HandleFunc("/json/details", jsonDetailsHandler).Methods("GET")
	router.HandleFunc("/json/ignores", jsonIgnoresHandler).Methods("GET")
	router.HandleFunc("/json/ignores/add/", jsonIgnoresAddHandler).Methods("POST")
	router.HandleFunc("/json/ignores/del/{id}", jsonIgnoresDeleteHandler).Methods("POST")
	router.HandleFunc("/json/ignores/save/{id}", jsonIgnoresUpdateHandler).Methods("POST")
	router.HandleFunc("/json/triage", jsonTriageHandler).Methods("POST")
	router.HandleFunc("/json/clusterdiff", jsonClusterDiffHandler).Methods("GET")
	router.HandleFunc("/json/triagelog", jsonTriageLogHandler).Methods("GET")
	router.HandleFunc("/json/triagelog/undo", jsonTriageUndoHandler).Methods("POST")
	router.HandleFunc("/json/trybot", jsonListTrybotsHandler).Methods("GET")
	router.HandleFunc("/json/failure", jsonListFailureHandler).Methods("GET")
	router.HandleFunc("/json/failure/clear", jsonClearFailureHandler).Methods("POST")

	// For everything else serve the same markup.
	indexFile := *resourcesDir + "/index.html"
	router.PathPrefix("/").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, indexFile)
	})

	// Add the necessary middleware and have the router handle all requests.
	// By structuring the middleware this way we only log requests that are
	// authenticated.
	rootHandler := httputils.LoggingGzipRequestResponse(router)
	if *forceLogin {
		rootHandler = login.ForceAuth(rootHandler, OAUTH2_CALLBACK_PATH)
	}

	// The jsonStatusHandler is being polled, so we exclude it from logging.
	http.HandleFunc("/json/trstatus", jsonStatusHandler)
	http.Handle("/", rootHandler)

	// Start the server
	glog.Infoln("Serving on http://127.0.0.1" + *port)
	glog.Fatal(http.ListenAndServe(*port, nil))
}
