// search contains the core functionality for searching for digests across a tile.
package search

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"

	"go.skia.org/infra/go/tiling"
	"go.skia.org/infra/go/util"
	"go.skia.org/infra/golden/go/blame"
	"go.skia.org/infra/golden/go/diff"
	"go.skia.org/infra/golden/go/digesttools"
	"go.skia.org/infra/golden/go/expstorage"
	"go.skia.org/infra/golden/go/goldingestion"
	"go.skia.org/infra/golden/go/ignore"
	"go.skia.org/infra/golden/go/indexer"
	"go.skia.org/infra/golden/go/storage"
	"go.skia.org/infra/golden/go/tally"
	"go.skia.org/infra/golden/go/trybot"
	"go.skia.org/infra/golden/go/types"
)

// Point is a single point. Used in Trace.
type Point struct {
	X int `json:"x"` // The commit index [0-49].
	Y int `json:"y"`
	S int `json:"s"` // Status of the digest: 0 if the digest matches our search, 1-8 otherwise.
}

// Trace describes a single trace, used in Traces.
type Trace struct {
	Data   []Point           `json:"data"`  // One Point for each test result.
	ID     string            `json:"label"` // The id of the trace. Keep the json as label to be compatible with dots-sk.
	Params map[string]string `json:"params"`
}

// DigestStatus is a digest and its status, used in Traces.
type DigestStatus struct {
	Digest string `json:"digest"`
	Status string `json:"status"`
}

// Traces is info about a group of traces. Used in Digest.
type Traces struct {
	TileSize int            `json:"tileSize"`
	Traces   []Trace        `json:"traces"`  // The traces where this digest appears.
	Digests  []DigestStatus `json:"digests"` // The other digests that appear in Traces.
}

// DiffDigest is information about a digest different from the one in Digest.
type DiffDigest struct {
	Closest  *digesttools.Closest `json:"closest"`
	ParamSet map[string][]string  `json:"paramset"`
}

// Diff is only populated for digests that are untriaged?
// Might still be useful to find diffs to closest pos for a neg, and vice-versa.
// Will also be useful if we ever get a canonical trace or centroid.
type Diff struct {
	Diff float32 `json:"diff"` // The smaller of the Pos and Neg diff.

	// Either may be nil if there's no positive or negative to compare against.
	Pos *DiffDigest `json:"pos"`
	Neg *DiffDigest `json:"neg"`
	//Centroid *DiffDigest

	Blame *blame.BlameDistribution `json:"blame"`
}

// Digest's are returned from Search, one for each match to Query.
type Digest struct {
	Test     string              `json:"test"`
	Digest   string              `json:"digest"`
	Status   string              `json:"status"`
	ParamSet map[string][]string `json:"paramset"`
	Traces   *Traces             `json:"traces"`
	Diff     *Diff               `json:"diff"`
}

// CommitRange is a range of commits, starting at the git hash Begin and ending at End, inclusive.
//
// Currently unimplemented in search.
type CommitRange struct {
	Begin string
	End   string
}

// Query is the query that Search understands.
type Query struct {
	BlameGroupID   string // Only applies to Untriaged digests.
	Pos            bool
	Neg            bool
	Unt            bool
	Head           bool
	IncludeIgnores bool
	Query          url.Values
	Issue          string
	Patchsets      []string
	CommitRange    CommitRange
	Limit          int  // Only return this many items.
	IncludeMaster  bool // Include digests from master when searching Rietveld issues.
}

// SearchResponse is the standard search response. Depending on the query some fields
// might be empty, i.e. IssueDetails only makes sense if a trybot isssue was given in the query.
type SearchResponse struct {
	Digests       []*Digest
	Total         int
	Commits       []*tiling.Commit
	IssueResponse *IssueResponse
}

// IssueResponse contains specific query responses when we search for a trybot issue. Currently
// it extends trybot.IssueDetails.
type IssueResponse struct {
	*trybot.IssueDetails
	QueryPatchsets []string
}

// excludeClassification returns true if the given label/status for a digest
// should be excluded based on the values in the query.
func (q *Query) excludeClassification(cl types.Label) bool {
	return ((cl == types.NEGATIVE) && !q.Neg) ||
		((cl == types.POSITIVE) && !q.Pos) ||
		((cl == types.UNTRIAGED) && !q.Unt)
}

// intermediate is the intermediate representation of the results coming from Search.
//
// To avoid filtering through the tile more than once we first take a pass
// through the tile and collect all info for the current Query, then we
// transform each intermediate into a Digest.
type intermediate struct {
	Test   string
	Digest string
	Traces map[string]tiling.Trace
}

func (i *intermediate) addTrace(id string, tr tiling.Trace, digests []string) {
	i.Traces[id] = tr
}

func newIntermediate(test, digest, id string, tr tiling.Trace, digests []string) *intermediate {
	ret := &intermediate{
		Test:   test,
		Digest: digest,
		Traces: map[string]tiling.Trace{},
	}
	ret.addTrace(id, tr, digests)
	return ret
}

// DigestSlice is a utility type for sorting slices of Digest by their max diff.
type DigestSlice []*Digest

func (p DigestSlice) Len() int           { return len(p) }
func (p DigestSlice) Less(i, j int) bool { return p[i].Diff.Diff > p[j].Diff.Diff }
func (p DigestSlice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

// Search returns a slice of Digests that match the input query, and the total number of Digests
// that matched the query. It also returns a slice of Commits that were used in the calculations.
func Search(q *Query, storages *storage.Storage, idx *indexer.SearchIndex) (*SearchResponse, error) {
	tile := idx.GetTile(q.IncludeIgnores)

	e, err := storages.ExpectationsStore.Get()
	if err != nil {
		return nil, fmt.Errorf("Couldn't get expectations: %s", err)
	}

	var ret []*Digest
	var issueResponse *IssueResponse = nil
	var commits []*tiling.Commit = nil
	if q.Issue != "" {
		ret, issueResponse, err = searchByIssue(q.Issue, q, e, q.Query, storages, idx)
	} else {
		ret, commits, err = searchTile(q, e, q.Query, storages, tile, idx)
	}

	if err != nil {
		return nil, err
	}

	sort.Sort(DigestSlice(ret))
	fullLength := len(ret)
	if fullLength > q.Limit {
		ret = ret[0:q.Limit]
	}

	return &SearchResponse{
		Digests:       ret,
		Total:         fullLength,
		Commits:       commits,
		IssueResponse: issueResponse,
	}, nil
}

// issueIntermediate is a utility struct for searchByIssue.
type issueIntermediate struct {
	test     string
	digest   string
	status   types.Label
	paramSet map[string][]string
}

// newIssueIntermediate creates a new instance of issueIntermediate.
func newIssueIntermediate(params map[string]string, digest string, status types.Label) *issueIntermediate {
	ret := &issueIntermediate{
		test:     params[types.PRIMARY_KEY_FIELD],
		digest:   digest,
		status:   status,
		paramSet: map[string][]string{},
	}
	ret.add(params)
	return ret
}

// add adds to an existing intermediate value.
func (i *issueIntermediate) add(params map[string]string) {
	util.AddParamsToParamSet(i.paramSet, params)
}

func searchByIssue(issueID string, q *Query, exp *expstorage.Expectations, parsedQuery url.Values, storages *storage.Storage, idx *indexer.SearchIndex) ([]*Digest, *IssueResponse, error) {
	issue, tile, err := storages.TrybotResults.GetIssue(issueID, q.Patchsets)
	if err != nil {
		return nil, nil, err
	}

	if issue == nil {
		return nil, nil, fmt.Errorf("Issue not found.")
	}

	// Get a matcher for the ignore rules if we filter ignores.
	var ignoreMatcher ignore.RuleMatcher = nil
	if !q.IncludeIgnores {
		ignoreMatcher, err = storages.IgnoreStore.BuildRuleMatcher()
		if err != nil {
			return nil, nil, fmt.Errorf("Unable to build rules matcher: %s", err)
		}
	}

	// Set up a rule to match the query.
	var queryRule ignore.QueryRule = nil
	if len(parsedQuery) > 0 {
		queryRule = ignore.NewQueryRule(parsedQuery)
	}

	pidMap := util.NewStringSet(issue.TargetPatchsets)
	talliesByTest := idx.TalliesByTest()
	digestMap := map[string]*Digest{}
	reviewURL := storages.RietveldAPI.Url()

	for idx, cid := range issue.CommitIDs {
		_, pid := goldingestion.ExtractIssueInfo(cid.CommitID, reviewURL)
		if !pidMap[pid] {
			continue
		}

		for _, trace := range tile.Traces {
			gTrace := trace.(*types.GoldenTrace)
			digest := gTrace.Values[idx]

			if digest == types.MISSING_DIGEST {
				continue
			}

			testName := gTrace.Params_[types.PRIMARY_KEY_FIELD]
			params := gTrace.Params_

			// 	If we have seen this before process it.
			key := testName + ":" + digest
			if found, ok := digestMap[key]; ok {
				util.AddParamsToParamSet(found.ParamSet, params)
				continue
			}

			// Should this trace be ignored.
			if ignoreMatcher != nil {
				if _, ok := ignoreMatcher(params); ok {
					continue
				}
			}

			// Does it match a given query.
			if (queryRule == nil) || queryRule.IsMatch(params) {
				if !q.IncludeMaster {
					if _, ok := talliesByTest[testName][digest]; ok {
						continue
					}
				}

				if cl := exp.Classification(testName, digest); !q.excludeClassification(cl) {
					digestMap[key] = &Digest{
						Test:     testName,
						Digest:   digest,
						ParamSet: util.AddParamsToParamSet(make(map[string][]string, len(params)), params),
						Status:   cl.String(),
					}
				}
			}
		}
	}

	ret := make([]*Digest, 0, len(digestMap))
	allDigests := make([]string, len(digestMap))
	emptyTraces := &Traces{}
	for _, digestEntry := range digestMap {
		digestEntry.Diff = buildDiff(digestEntry.Test, digestEntry.Digest, exp, nil, talliesByTest, storages.DiffStore, idx, q.IncludeIgnores)
		digestEntry.Traces = emptyTraces
		ret = append(ret, digestEntry)
		allDigests = append(allDigests, digestEntry.Digest)
	}

	loadDigests(storages.DiffStore, allDigests)

	issueResponse := &IssueResponse{
		IssueDetails:   issue,
		QueryPatchsets: issue.TargetPatchsets,
	}

	return ret, issueResponse, nil
}

// searchTile queries across a tile.
func searchTile(q *Query, e *expstorage.Expectations, parsedQuery url.Values, storages *storage.Storage, tile *tiling.Tile, idx *indexer.SearchIndex) ([]*Digest, []*tiling.Commit, error) {
	// TODO Use CommitRange to create a trimmed tile.

	traceTally := idx.TalliesByTrace()
	lastCommitIndex := tile.LastCommitIndex()

	// Loop over the tile and pull out all the digests that match
	// the query, collecting the matching traces as you go. Build
	// up a set of intermediate's that can then be used to calculate
	// Digest's.

	// map [test:digest] *intermediate
	inter := map[string]*intermediate{}
	for id, tr := range tile.Traces {
		if tiling.Matches(tr, parsedQuery) {
			test := tr.Params()[types.PRIMARY_KEY_FIELD]
			// Get all the digests
			digests := digestsFromTrace(id, tr, q.Head, lastCommitIndex, traceTally)
			for _, digest := range digests {
				cl := e.Classification(test, digest)
				if q.excludeClassification(cl) {
					continue
				}

				// Fix blamer to make this easier.
				if q.BlameGroupID != "" {
					if cl == types.UNTRIAGED {
						b := idx.GetBlame(test, digest, tile.Commits)
						if q.BlameGroupID != blameGroupID(b, tile.Commits) {
							continue
						}
					} else {
						continue
					}
				}
				key := fmt.Sprintf("%s:%s", test, digest)
				if i, ok := inter[key]; !ok {
					inter[key] = newIntermediate(test, digest, id, tr, digests)
				} else {
					i.addTrace(id, tr, digests)
				}
			}
		}
	}
	// Now loop over all the intermediates and build a Digest for each one.
	ret := make([]*Digest, 0, len(inter))
	for key, i := range inter {
		parts := strings.Split(key, ":")
		ret = append(ret, digestFromIntermediate(parts[0], parts[1], i, e, tile, idx, storages.DiffStore, q.IncludeIgnores))
	}
	return ret, tile.Commits, nil
}

func digestFromIntermediate(test, digest string, inter *intermediate, e *expstorage.Expectations, tile *tiling.Tile, idx *indexer.SearchIndex, diffStore diff.DiffStore, includeIgnores bool) *Digest {
	traceTally := idx.TalliesByTrace()
	ret := &Digest{
		Test:     test,
		Digest:   digest,
		Status:   e.Classification(test, digest).String(),
		ParamSet: idx.GetParamsetSummary(test, digest, includeIgnores),
		Traces:   buildTraces(test, digest, inter.Traces, e, tile, traceTally),
		Diff:     buildDiff(test, digest, e, tile, idx.TalliesByTest(), diffStore, idx, includeIgnores),
	}
	return ret
}

// buildDiff creates a Diff for the given intermediate.
func buildDiff(test, digest string, e *expstorage.Expectations, tile *tiling.Tile, testTally map[string]tally.Tally, diffStore diff.DiffStore, idx *indexer.SearchIndex, includeIgnores bool) *Diff {
	ret := &Diff{
		Diff: math.MaxFloat32,
		Pos:  nil,
		Neg:  nil,
	}

	if tile != nil {
		ret.Blame = idx.GetBlame(test, digest, tile.Commits)
	}

	t := testTally[test]
	if t == nil {
		t = tally.Tally{}
	}

	var diffVal float32 = 0
	if closest := digesttools.ClosestDigest(test, digest, e, t, diffStore, types.POSITIVE); closest.Digest != "" {
		ret.Pos = &DiffDigest{
			Closest: closest,
		}
		ret.Pos.ParamSet = idx.GetParamsetSummary(test, ret.Pos.Closest.Digest, includeIgnores)
		diffVal = closest.Diff
	}

	if closest := digesttools.ClosestDigest(test, digest, e, t, diffStore, types.NEGATIVE); closest.Digest != "" {
		ret.Neg = &DiffDigest{
			Closest: closest,
		}
		ret.Neg.ParamSet = idx.GetParamsetSummary(test, ret.Neg.Closest.Digest, includeIgnores)
		if (ret.Pos == nil) || (closest.Diff < diffVal) {
			diffVal = closest.Diff
		}
	}

	ret.Diff = diffVal
	return ret
}

// buildTraces returns a Trace for the given intermediate.
func buildTraces(test, digest string, traces map[string]tiling.Trace, e *expstorage.Expectations, tile *tiling.Tile, traceTally map[string]tally.Tally) *Traces {
	traceNames := make([]string, 0, len(traces))
	for id, _ := range traces {
		traceNames = append(traceNames, id)
	}

	ret := &Traces{
		TileSize: len(tile.Commits),
		Traces:   []Trace{},
		Digests:  []DigestStatus{},
	}

	sort.Strings(traceNames)

	last := tile.LastCommitIndex()
	y := 0
	if len(traceNames) > 0 {
		ret.Digests = append(ret.Digests, DigestStatus{
			Digest: digest,
			Status: e.Classification(test, digest).String(),
		})
	}
	for _, id := range traceNames {
		t, ok := traceTally[id]
		if !ok {
			continue
		}
		if count, ok := t[digest]; !ok || count == 0 {
			continue
		}
		trace := traces[id].(*types.GoldenTrace)
		p := Trace{
			Data:   []Point{},
			ID:     id,
			Params: trace.Params(),
		}
		for i := last; i >= 0; i-- {
			if trace.IsMissing(i) {
				continue
			}
			// s is the status of the digest, it is either 0 for a match, or [1-8] if not.
			s := 0
			if trace.Values[i] != digest {
				if index := digestIndex(trace.Values[i], ret.Digests); index != -1 {
					s = index
				} else {
					if len(ret.Digests) < 9 {
						d := trace.Values[i]
						ret.Digests = append(ret.Digests, DigestStatus{
							Digest: d,
							Status: e.Classification(test, d).String(),
						})
						s = len(ret.Digests) - 1
					} else {
						s = 8
					}
				}
			}
			p.Data = append(p.Data, Point{
				X: i,
				Y: y,
				S: s,
			})
		}
		sort.Sort(PointSlice(p.Data))
		ret.Traces = append(ret.Traces, p)
		y += 1
	}

	return ret
}

// digestIndex returns the index of the digest d in digestInfo, or -1 if not found.
func digestIndex(d string, digestInfo []DigestStatus) int {
	for i, di := range digestInfo {
		if di.Digest == d {
			return i
		}
	}
	return -1
}

// blameGroupID takes a blame distrubution with just indices of commits and
// returns an id for the blame group, which is just a string, the concatenated
// git hashes in commit time order.
func blameGroupID(b *blame.BlameDistribution, commits []*tiling.Commit) string {
	ret := []string{}
	for _, index := range b.Freq {
		ret = append(ret, commits[index].Hash)
	}
	return strings.Join(ret, ":")
}

// digestsFromTrace returns all the digests in the given trace, controlled by
// 'head', and being robust to tallies not having been calculated for the
// trace.
func digestsFromTrace(id string, tr tiling.Trace, head bool, lastCommitIndex int, traceTally map[string]tally.Tally) []string {
	digests := util.NewStringSet()
	if head {
		// Find the last non-missing value in the trace.
		for i := lastCommitIndex; i >= 0; i-- {
			if tr.IsMissing(i) {
				continue
			} else {
				digests[tr.(*types.GoldenTrace).Values[i]] = true
				break
			}
		}
	} else {
		// Use the traceTally if available, otherwise just inspect the trace.
		if t, ok := traceTally[id]; ok {
			for k, _ := range t {
				digests[k] = true
			}
		} else {
			for i := lastCommitIndex; i >= 0; i-- {
				if !tr.IsMissing(i) {
					digests[tr.(*types.GoldenTrace).Values[i]] = true
				}
			}
		}
	}

	return digests.Keys()
}

// PointSlice is a utility type for sorting Points by their X value.
type PointSlice []Point

func (p PointSlice) Len() int           { return len(p) }
func (p PointSlice) Less(i, j int) bool { return p[i].X < p[j].X }
func (p PointSlice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

// TODO(stephana): Replace digesttools.Closest here and above with an overall
// consolidated structure to measure the distance between two digests.

// DigestDiff contains the result of comparing two digests.
type DigestDiff struct {
	Left  *Digest              `json:"left"`  // Diff element assumed to be nil
	Right *Digest              `json:"right"` // Diff element assumed to be nil
	Diff  *digesttools.Closest `json:"diff"`  // Digest field assumed to be nil
}

// CompareDigests compares two digests that were generated by the given test. It returns
// an instance of DigestDiff.
func CompareDigests(test, left, right string, storages *storage.Storage, idx *indexer.SearchIndex) (*DigestDiff, error) {
	// Get the diff between the two digests
	diff, err := storages.DiffStore.Get(left, []string{right})
	if err != nil {
		return nil, err
	}

	exp, err := storages.ExpectationsStore.Get()
	if err != nil {
		return nil, err
	}

	return &DigestDiff{
		Left: &Digest{
			Test:     test,
			Digest:   left,
			Status:   exp.Classification(test, left).String(),
			ParamSet: idx.GetParamsetSummary(test, left, true),
		},
		Right: &Digest{
			Test:     test,
			Digest:   right,
			Status:   exp.Classification(test, right).String(),
			ParamSet: idx.GetParamsetSummary(test, right, true),
		},
		Diff: digesttools.ClosestFromDiffMetrics(diff[right]),
	}, nil
}

// DigestDetails contains details about a digest.
type DigestDetails struct {
	Digest  *Digest          `json:"digest"`
	Commits []*tiling.Commit `json:"commits"`
}

// GetDigestDetails returns details about a digest as an instance of DigestDetails.
func GetDigestDetails(test, digest string, storages *storage.Storage, idx *indexer.SearchIndex) (*DigestDetails, error) {
	tile := idx.GetTile(true)

	exp, err := storages.ExpectationsStore.Get()
	if err != nil {
		return nil, err
	}

	traces := map[string]tiling.Trace{}
	for traceId, trace := range tile.Traces {
		if trace.Params()[types.PRIMARY_KEY_FIELD] != test {
			continue
		}
		gTrace := trace.(*types.GoldenTrace)
		for _, val := range gTrace.Values {
			if val == digest {
				traces[traceId] = trace
			}
		}
	}

	return &DigestDetails{
		Digest: &Digest{
			Test:     test,
			Digest:   digest,
			Status:   exp.Classification(test, digest).String(),
			ParamSet: idx.GetParamsetSummary(test, digest, true),
			Traces:   buildTraces(test, digest, traces, exp, tile, idx.TalliesByTrace()),
			Diff:     buildDiff(test, digest, exp, nil, idx.TalliesByTest(), storages.DiffStore, idx, true),
		},
		Commits: tile.Commits,
	}, nil
}

// TODO(stephana): Remove this stop gap when a better diff store is in place.

// loadDigests makes sure the digests are loaded.
func loadDigests(diffStore diff.DiffStore, digests []string) {
	go func() {
		diffStore.AbsPath(digests)
	}()
}
