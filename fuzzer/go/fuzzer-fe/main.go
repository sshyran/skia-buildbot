package main

/*
Runs the frontend portion of the fuzzer.  This primarily is the webserver (see DESIGN.md)
*/

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/gorilla/mux"
	"github.com/skia-dev/glog"
	fcommon "go.skia.org/infra/fuzzer/go/common"
	"go.skia.org/infra/fuzzer/go/config"
	"go.skia.org/infra/fuzzer/go/data"
	"go.skia.org/infra/fuzzer/go/frontend"
	"go.skia.org/infra/fuzzer/go/frontend/gsloader"
	"go.skia.org/infra/fuzzer/go/frontend/syncer"
	"go.skia.org/infra/fuzzer/go/fuzzcache"
	"go.skia.org/infra/fuzzer/go/issues"
	"go.skia.org/infra/go/auth"
	"go.skia.org/infra/go/common"
	"go.skia.org/infra/go/fileutil"
	"go.skia.org/infra/go/gitinfo"
	"go.skia.org/infra/go/gs"
	"go.skia.org/infra/go/httputils"
	"go.skia.org/infra/go/influxdb"
	"go.skia.org/infra/go/login"
	"go.skia.org/infra/go/skiaversion"
	"go.skia.org/infra/go/util"
	"go.skia.org/infra/go/vcsinfo"
	"golang.org/x/net/context"
	"google.golang.org/api/option"
)

const (
	// OAUTH2_CALLBACK_PATH is callback endpoint used for the Oauth2 flow.
	OAUTH2_CALLBACK_PATH = "/oauth2callback/"
)

var (
	// indexTemplate is the main index.html page we serve.
	indexTemplate *template.Template = nil
	// rollTemplate is used for /roll, which allows a user to roll the fuzzer forward.
	rollTemplate *template.Template = nil
	// detailsTemplate is used for /category, which displays the count of fuzzes in various files
	// as well as the stacktraces.
	detailsTemplate *template.Template = nil

	storageClient *storage.Client = nil

	versionWatcher *fcommon.VersionWatcher = nil

	fuzzSyncer *syncer.FuzzSyncer = nil

	issueManager *issues.IssuesManager = nil

	repo     *gitinfo.GitInfo = nil
	repoLock sync.Mutex
)

var (
	// web server params
	influxHost     = flag.String("influxdb_host", influxdb.DEFAULT_HOST, "The InfluxDB hostname.")
	influxUser     = flag.String("influxdb_name", influxdb.DEFAULT_USER, "The InfluxDB username.")
	influxPassword = flag.String("influxdb_password", influxdb.DEFAULT_PASSWORD, "The InfluxDB password.")
	influxDatabase = flag.String("influxdb_database", influxdb.DEFAULT_DATABASE, "The InfluxDB database.")

	host         = flag.String("host", "localhost", "HTTP service host")
	port         = flag.String("port", ":8001", "HTTP service port (e.g., ':8002')")
	local        = flag.Bool("local", false, "Running locally if true. As opposed to in production.")
	resourcesDir = flag.String("resources_dir", "", "The directory to find templates, JS, and CSS files. If blank the current directory will be used.")
	boltDBPath   = flag.String("bolt_db_path", "fuzzer-db", "The path to the bolt db to be used as a local cache.")

	// OAUTH params
	authWhiteList = flag.String("auth_whitelist", login.DEFAULT_DOMAIN_WHITELIST, "White space separated list of domains and email addresses that are allowed to login.")
	redirectURL   = flag.String("redirect_url", "https://fuzzer.skia.org/oauth2callback/", "OAuth2 redirect url. Only used when local=false.")

	// Scanning params
	// At the moment, the front end does not actually build Skia.  It checks out Skia to get
	// commit information only.  However, it is still not a good idea to share SkiaRoot dirs.
	skiaRoot            = flag.String("skia_root", "", "[REQUIRED] The root directory of the Skia source code.  Cannot be safely shared with backend.")
	clangPath           = flag.String("clang_path", "", "[REQUIRED] The path to the clang executable.")
	clangPlusPlusPath   = flag.String("clang_p_p_path", "", "[REQUIRED] The path to the clang++ executable.")
	depotToolsPath      = flag.String("depot_tools_path", "", "The absolute path to depot_tools.  Can be empty if they are on your path.")
	executableCachePath = flag.String("executable_cache_path", filepath.Join(os.TempDir(), "executable_cache"), "The path in which built fuzz executables can be cached.  Can be safely shared with backend.")
	bucket              = flag.String("bucket", "skia-fuzzer", "The GCS bucket in which to locate found fuzzes.")
	downloadProcesses   = flag.Int("download_processes", 4, "The number of download processes to be used for fetching fuzzes.")

	// Other params
	versionCheckPeriod = flag.Duration("version_check_period", 20*time.Second, `The period used to check the version of Skia that needs fuzzing.`)
	fuzzSyncPeriod     = flag.Duration("fuzz_sync_period", 2*time.Minute, `The period used to sync bad fuzzes and check the count of grey and bad fuzzes.`)
)

var requiredFlags = []string{"skia_root", "clang_path", "clang_p_p_path", "bolt_db_path", "executable_cache_path"}

func Init() {
	reloadTemplates()
}

func reloadTemplates() {
	indexTemplate = template.Must(template.ParseFiles(
		filepath.Join(*resourcesDir, "templates/index.html"),
		filepath.Join(*resourcesDir, "templates/header.html"),
	))
	rollTemplate = template.Must(template.ParseFiles(
		filepath.Join(*resourcesDir, "templates/roll.html"),
		filepath.Join(*resourcesDir, "templates/header.html"),
	))
	detailsTemplate = template.New("details.html")
	// Allows this template to have Polymer binding in it and go template markup.  The go templates
	// have been changed to be {%.Thing%} instead of {{.Thing}}
	detailsTemplate.Delims("{%", "%}")
	detailsTemplate = template.Must(detailsTemplate.ParseFiles(
		filepath.Join(*resourcesDir, "templates/details.html"),
		filepath.Join(*resourcesDir, "templates/header.html"),
	))
}

func main() {
	defer common.LogPanic()
	// Calls flag.Parse()
	common.InitWithMetrics2("fuzzer-fe", influxHost, influxUser, influxPassword, influxDatabase, local)

	if err := writeFlagsToConfig(); err != nil {
		glog.Fatalf("Problem with configuration: %s", err)
	}

	Init()

	if err := setupOAuth(); err != nil {
		glog.Fatal(err)
	}

	go func() {
		if err := fcommon.DownloadSkiaVersionForFuzzing(storageClient, config.Common.SkiaRoot, &config.Common, !*local); err != nil {
			glog.Fatalf("Problem downloading Skia: %s", err)
		}

		fuzzSyncer = syncer.New(storageClient)
		fuzzSyncer.Start()

		cache, err := fuzzcache.New(config.FrontEnd.BoltDBPath)
		if err != nil {
			glog.Fatalf("Could not create fuzz report cache at %s: %s", config.FrontEnd.BoltDBPath, err)
		}
		defer util.Close(cache)

		if err := gsloader.LoadFromBoltDB(cache); err != nil {
			glog.Errorf("Could not load from boltdb.  Loading from source of truth anyway. %s", err)
		}
		gsLoader := gsloader.New(storageClient, cache)
		if err := gsLoader.LoadFreshFromGoogleStorage(); err != nil {
			glog.Fatalf("Error loading in data from GCS: %s", err)
		}
		fuzzSyncer.SetGSLoader(gsLoader)
		updater := frontend.NewVersionUpdater(gsLoader, fuzzSyncer)
		versionWatcher = fcommon.NewVersionWatcher(storageClient, config.Common.VersionCheckPeriod, nil, updater.HandleCurrentVersion)
		versionWatcher.Start()

		err = <-versionWatcher.Status
		glog.Fatal(err)
	}()
	runServer()
}

func writeFlagsToConfig() error {
	// Check the required ones and terminate if they are not provided
	for _, f := range requiredFlags {
		if flag.Lookup(f).Value.String() == "" {
			return fmt.Errorf("Required flag %s is empty.", f)
		}
	}
	var err error
	config.Common.SkiaRoot, err = fileutil.EnsureDirExists(*skiaRoot)
	if err != nil {
		return err
	}
	config.Common.ExecutableCachePath, err = fileutil.EnsureDirExists(*executableCachePath)
	if err != nil {
		return err
	}
	config.FrontEnd.BoltDBPath = *boltDBPath
	config.Common.VersionCheckPeriod = *versionCheckPeriod
	config.Common.ClangPath = *clangPath
	config.Common.ClangPlusPlusPath = *clangPlusPlusPath
	config.Common.DepotToolsPath = *depotToolsPath

	config.GS.Bucket = *bucket
	config.FrontEnd.NumDownloadProcesses = *downloadProcesses
	config.FrontEnd.FuzzSyncPeriod = *fuzzSyncPeriod
	return nil
}

func setupOAuth() error {
	var useRedirectURL = fmt.Sprintf("http://localhost%s/oauth2callback/", *port)
	if !*local {
		useRedirectURL = *redirectURL
	}
	if err := login.InitFromMetadataOrJSON(useRedirectURL, login.DEFAULT_SCOPE, *authWhiteList); err != nil {
		return fmt.Errorf("Problem setting up server OAuth: %s", err)
	}

	client, err := auth.NewDefaultJWTServiceAccountClient(auth.SCOPE_READ_WRITE)
	if err != nil {
		return fmt.Errorf("Problem setting up client OAuth: %s", err)
	}

	storageClient, err = storage.NewClient(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		return fmt.Errorf("Problem authenticating: %s", err)
	}

	issueManager = issues.NewManager(client)
	return nil
}

func runServer() {
	serverURL := "https://" + *host
	if *local {
		serverURL = "http://" + *host + *port
	}

	r := mux.NewRouter()
	r.PathPrefix("/res/").HandlerFunc(httputils.MakeResourceHandler(*resourcesDir))

	r.HandleFunc(OAUTH2_CALLBACK_PATH, login.OAuth2CallbackHandler)
	r.HandleFunc("/", indexHandler)
	r.HandleFunc("/category/{category:[a-z_]+}", detailsPageHandler)
	r.HandleFunc("/category/{category:[a-z_]+}/name/{name}", detailsPageHandler)
	r.HandleFunc("/category/{category:[a-z_]+}/file/{file}", detailsPageHandler)
	r.HandleFunc("/category/{category:[a-z_]+}/file/{file}/func/{function}", detailsPageHandler)
	r.HandleFunc(`/category/{category:[a-z_]+}/file/{file}/func/{function}/line/{line}`, detailsPageHandler)
	r.HandleFunc("/loginstatus/", login.StatusHandler)
	r.HandleFunc("/logout/", login.LogoutHandler)
	r.HandleFunc("/json/version", skiaversion.JsonHandler)
	r.HandleFunc("/json/fuzz-summary", summaryJSONHandler)
	r.HandleFunc("/json/details", detailsJSONHandler)
	r.HandleFunc("/json/status", statusJSONHandler)
	r.HandleFunc(`/fuzz/{category:[a-z_]+}/{name:[0-9a-f]+}`, fuzzHandler)
	r.HandleFunc(`/metadata/{category:[a-z_]+}/{name:[0-9a-f]+_(debug|release)\.(err|dump|asan)}`, metadataHandler)
	r.HandleFunc("/newBug", newBugHandler)
	r.HandleFunc("/roll", rollHandler)
	r.HandleFunc("/roll/revision", updateRevision)

	rootHandler := login.ForceAuth(httputils.LoggingGzipRequestResponse(r), OAUTH2_CALLBACK_PATH)

	http.Handle("/", rootHandler)
	glog.Infof("Ready to serve on %s", serverURL)
	glog.Fatal(http.ListenAndServe(*port, nil))
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if *local {
		reloadTemplates()
	}
	w.Header().Set("Content-Type", "text/html")

	if err := indexTemplate.Execute(w, nil); err != nil {
		glog.Errorf("Failed to expand template: %v", err)
	}
}

func detailsPageHandler(w http.ResponseWriter, r *http.Request) {
	if *local {
		reloadTemplates()
	}
	w.Header().Set("Content-Type", "text/html")

	var cat = struct {
		Category string
	}{
		Category: mux.Vars(r)["category"],
	}

	if err := detailsTemplate.Execute(w, cat); err != nil {
		glog.Errorf("Failed to expand template: %v", err)
	}
}

func rollHandler(w http.ResponseWriter, r *http.Request) {
	if *local {
		reloadTemplates()
	}
	w.Header().Set("Content-Type", "text/html")

	if err := rollTemplate.Execute(w, nil); err != nil {
		glog.Errorf("Failed to expand template: %v", err)
	}
}

type countSummary struct {
	Category        string `json:"category"`
	CategoryDisplay string `json:"categoryDisplay"`
	TotalBad        int    `json:"totalBadCount"`
	TotalGrey       int    `json:"totalGreyCount"`
	// "This" means "newly introduced/fixed in this revision"
	ThisBad        int    `json:"thisBadCount"`
	ThisRegression int    `json:"thisRegressionCount"`
	Status         string `json:"status"`
	Groomer        string `json:"groomer"`
}

func summaryJSONHandler(w http.ResponseWriter, r *http.Request) {
	summary := getSummary()

	if err := json.NewEncoder(w).Encode(summary); err != nil {
		glog.Errorf("Failed to write or encode output: %v", err)
		return
	}
}

func getSummary() []countSummary {
	counts := make([]countSummary, 0, len(fcommon.FUZZ_CATEGORIES))
	for _, cat := range fcommon.FUZZ_CATEGORIES {
		o := countSummary{
			CategoryDisplay: fcommon.PrettifyCategory(cat),
			Category:        cat,
		}
		c := syncer.FuzzCount{
			TotalBad:       -1,
			TotalGrey:      -1,
			ThisBad:        -1,
			ThisRegression: -1,
		}
		if fuzzSyncer != nil {
			c = fuzzSyncer.LastCount(cat)
		}
		o.TotalBad = c.TotalBad
		o.ThisBad = c.ThisBad
		o.TotalGrey = c.TotalGrey
		o.ThisRegression = c.ThisRegression
		o.Status = fcommon.Status(cat)
		o.Groomer = fcommon.Groomer(cat)
		counts = append(counts, o)
	}
	return counts
}

func detailsJSONHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	category := r.FormValue("category")
	name := r.FormValue("name")
	// The file names have "/" in them and the functions can have "(*&" in them.
	// We base64 encode them to prevent problems.
	file, err := decodeBase64(r.FormValue("file"))
	if err != nil {
		httputils.ReportError(w, r, err, "There was a problem decoding the params.")
		return
	}
	function, err := decodeBase64(r.FormValue("func"))
	if err != nil {
		httputils.ReportError(w, r, err, "There was a problem decoding the params.")
		return
	}
	lineStr, err := decodeBase64(r.FormValue("line"))
	if err != nil {
		httputils.ReportError(w, r, err, "There was a problem decoding the params.")
		return
	}

	var f data.FuzzReportTree
	if name != "" {
		var err error
		if f, err = data.FindFuzzDetailForFuzz(category, name); err != nil {
			httputils.ReportError(w, r, err, "There was a problem fulfilling the request.")
		}
	} else {
		line, err := strconv.ParseInt(lineStr, 10, 32)
		if err != nil {
			line = fcommon.UNKNOWN_LINE
		}

		if f, err = data.FindFuzzDetails(category, file, function, int(line)); err != nil {
			httputils.ReportError(w, r, err, "There was a problem fulfilling the request.")
			return
		}
	}

	if err := json.NewEncoder(w).Encode(f); err != nil {
		glog.Errorf("Failed to write or encode output: %s", err)
		return
	}
}

func decodeBase64(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	b, err := base64.URLEncoding.DecodeString(s)
	return string(b), err
}

func fuzzHandler(w http.ResponseWriter, r *http.Request) {
	v := mux.Vars(r)
	category := v["category"]
	// Check the category to avoid someone trying to download arbitrary files from our bucket
	if !fcommon.HasCategory(category) {
		httputils.ReportError(w, r, nil, "Category not found")
		return
	}
	name := v["name"]
	contents, err := gs.FileContentsFromGS(storageClient, config.GS.Bucket, fmt.Sprintf("%s/%s/bad/%s/%s", category, config.Common.SkiaVersion.Hash, name, name))
	if err != nil {
		httputils.ReportError(w, r, err, "Fuzz not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", name)
	n, err := w.Write(contents)
	if err != nil || n != len(contents) {
		glog.Errorf("Could only serve %d bytes of fuzz %s, not %d: %s", n, name, len(contents), err)
		return
	}
}

func metadataHandler(w http.ResponseWriter, r *http.Request) {
	v := mux.Vars(r)
	category := v["category"]
	// Check the category to avoid someone trying to download arbitrary files from our bucket
	if !fcommon.HasCategory(category) {
		httputils.ReportError(w, r, nil, "Category not found")
		return
	}
	name := v["name"]
	hash := strings.Split(name, "_")[0]

	contents, err := gs.FileContentsFromGS(storageClient, config.GS.Bucket, fmt.Sprintf("%s/%s/bad/%s/%s", category, config.Common.SkiaVersion.Hash, hash, name))
	if err != nil {
		httputils.ReportError(w, r, err, "Fuzz metadata not found")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", name)
	n, err := w.Write(contents)
	if err != nil || n != len(contents) {
		glog.Errorf("Could only serve %d bytes of metadata %s, not %d: %s", n, name, len(contents), err)
		return
	}
}

type commit struct {
	Hash   string `json:"hash"`
	Author string `json:"author"`
}

type status struct {
	Current     commit    `json:"current"`
	Pending     *commit   `json:"pending"`
	LastUpdated time.Time `json:"lastUpdated"`
}

func statusJSONHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	s := status{
		Current: commit{
			Hash:   "loading",
			Author: "(Loading)",
		},
		Pending: nil,
	}

	if config.Common.SkiaVersion != nil {
		s.Current.Hash = config.Common.SkiaVersion.Hash
		s.Current.Author = config.Common.SkiaVersion.Author
		s.LastUpdated = config.Common.SkiaVersion.Timestamp
		if versionWatcher != nil {
			if pending := versionWatcher.LastPendingHash; pending != "" {
				if ci, err := getCommitInfo(pending); err != nil {
					glog.Errorf("Problem getting git info about pending revision %s: %s", pending, err)
				} else {
					s.Pending = &commit{
						Hash:   ci.Hash,
						Author: ci.Author,
					}
				}
			}
		}
	}

	if err := json.NewEncoder(w).Encode(s); err != nil {
		glog.Errorf("Failed to write or encode output: %s", err)
		return
	}
}

func newBugHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	name := r.FormValue("name")
	category := r.FormValue("category")

	p := issues.IssueReportingPackage{
		Category:       category,
		FuzzName:       name,
		CommitRevision: config.Common.SkiaVersion.Hash,
	}
	if u, err := issueManager.CreateBadBugURL(p); err != nil {
		httputils.ReportError(w, r, err, fmt.Sprintf("Problem creating issue link %#v", p))
	} else {
		// 303 means "make a GET request to this url"
		http.Redirect(w, r, u, 303)
	}
}

// getCommitInfo updates the front end's checkout of Skia and then queries it for information about the given revision.
func getCommitInfo(revision string) (*vcsinfo.LongCommit, error) {
	repoLock.Lock()
	defer repoLock.Unlock()
	var err error
	repo, err = gitinfo.NewGitInfo(config.Common.SkiaRoot, true, false)
	if err != nil {
		return nil, fmt.Errorf("Could not fetch Skia before check: %s", err)
	}

	currInfo, err := repo.Details(revision, false)
	if err != nil || currInfo == nil {
		return nil, fmt.Errorf("Could not get info for %s: %s", revision, err)
	}
	return currInfo, nil
}

func updateRevision(w http.ResponseWriter, r *http.Request) {
	if !login.IsGoogler(r) {
		http.Error(w, "You do not have permission to push.  You must be a Googler.", http.StatusForbidden)
		return
	}
	var msg struct {
		Revision string `json:"revision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		httputils.ReportError(w, r, err, fmt.Sprintf("Failed to decode request body: %s", err))
		return
	}
	msg.Revision = strings.TrimSpace(msg.Revision)
	if msg.Revision == "" {
		http.Error(w, "Revision cannot be blank", http.StatusBadRequest)
		return
	}
	user := login.LoggedInAs(r)

	glog.Infof("User %s is trying to roll the fuzzer to revision %q", user, msg.Revision)

	if config.Common.SkiaVersion == nil || versionWatcher == nil || versionWatcher.LastCurrentHash == "" {
		http.Error(w, "The fuzzer isn't finished booting up.  Try again later.", http.StatusServiceUnavailable)
		return
	}
	if versionWatcher.LastPendingHash != "" {
		http.Error(w, "There is already a pending version.", http.StatusBadRequest)
		return
	}

	currInfo, err := getCommitInfo(versionWatcher.LastCurrentHash)
	if err != nil || currInfo == nil {
		httputils.ReportError(w, r, err, "Could not get information about current revision.  Please try again later")
		return
	}
	newInfo, err := getCommitInfo(msg.Revision)
	if err != nil || newInfo == nil {
		httputils.ReportError(w, r, err, "Could not get information about revision.  Are you sure it exists?")
		return
	}

	// We can only assume this to be the case because Skia has no branches that would
	// cause commits of a later time to actually be merged in before other commits.
	if newInfo.Timestamp.Before(currInfo.Timestamp) {
		http.Error(w, fmt.Sprintf("Revision cannot be before current revision %s at %s", currInfo.Hash, currInfo.Timestamp), http.StatusBadRequest)
		return
	}

	glog.Infof("Turning the crank to revision %q", newInfo.Hash)
	if err := frontend.UpdateVersionToFuzz(storageClient, config.GS.Bucket, newInfo.Hash); err != nil {
		glog.Errorf("Could not turn the crank: %s", err)
	} else {
		versionWatcher.Recheck()
	}
}
