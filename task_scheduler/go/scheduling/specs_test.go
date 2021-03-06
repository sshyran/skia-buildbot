package scheduling

import (
	"encoding/json"
	"path"
	"testing"
	"time"

	assert "github.com/stretchr/testify/require"
	"go.skia.org/infra/go/gitinfo"
	"go.skia.org/infra/go/testutils"
	"go.skia.org/infra/go/util"
)

func TestTaskSpecs(t *testing.T) {
	testutils.SkipIfShort(t)

	tr := util.NewTempRepo()
	defer tr.Cleanup()

	repos := gitinfo.NewRepoMap(tr.Dir)
	cache := newTaskCfgCache(repos)

	c1 := "81add9e329cde292667a1ce427007b5ff701fad1"
	c2 := "4595a2a2662d6cef863870ca68f64824c4b5ef2d"
	repo := "skia.git"
	specs, err := cache.GetTaskSpecsForCommits(map[string][]string{
		repo: []string{c1, c2},
	})
	assert.NoError(t, err)
	assert.Equal(t, 2, len(specs[repo]))

	// c1 has a Build and Test task.
	assert.Equal(t, 2, len(specs[repo][c1]))

	// c2 adds a Perf task.
	assert.Equal(t, 3, len(specs[repo][c2]))
}

func TestTaskCfgCacheCleanup(t *testing.T) {
	testutils.SkipIfShort(t)

	tr := util.NewTempRepo()
	defer tr.Cleanup()

	repos := gitinfo.NewRepoMap(tr.Dir)
	cache := newTaskCfgCache(repos)

	// Load configs into the cache.
	c1 := "81add9e329cde292667a1ce427007b5ff701fad1"
	c2 := "4595a2a2662d6cef863870ca68f64824c4b5ef2d"
	repo := "skia.git"
	_, err := cache.GetTaskSpecsForCommits(map[string][]string{
		repo: []string{c1, c2},
	})
	assert.NoError(t, err)
	assert.Equal(t, 2, len(cache.cache[repo]))

	// Cleanup, with a period intentionally designed to remove c1 but not c2.
	r, err := gitinfo.NewGitInfo(path.Join(tr.Dir, repo), false, false)
	assert.NoError(t, err)
	d1, err := r.Details(c1, false)
	assert.NoError(t, err)
	// c1 and c2 are about 5 seconds apart.
	period := time.Now().Sub(d1.Timestamp) - 2*time.Second
	assert.NoError(t, cache.Cleanup(period))
	assert.Equal(t, 1, len(cache.cache[repo]))
}

func TestTasksCircularDependency(t *testing.T) {
	makeTasksCfg := func(tasks map[string][]string) string {
		specs := make(map[string]*TaskSpec, len(tasks))
		for name, deps := range tasks {
			specs[name] = &TaskSpec{
				CipdPackages: []*CipdPackage{},
				Dependencies: deps,
				Dimensions:   []string{},
				Isolate:      "abc123",
				Priority:     0.0,
			}
		}
		cfg := TasksCfg{
			Tasks: specs,
		}
		c, err := json.Marshal(&cfg)
		assert.NoError(t, err)
		return string(c)
	}

	// Bonus: Unknown dependency.
	_, err := ParseTasksCfg(makeTasksCfg(map[string][]string{
		"a": []string{"b"},
	}))
	assert.EqualError(t, err, "Task \"a\" has unknown task \"b\" as a dependency.")

	// No tasks.
	_, err = ParseTasksCfg(makeTasksCfg(map[string][]string{}))
	assert.NoError(t, err)

	// Single-node cycle.
	_, err = ParseTasksCfg(makeTasksCfg(map[string][]string{
		"a": []string{"a"},
	}))
	assert.EqualError(t, err, "Found a circular dependency involving \"a\" and \"a\"")

	// Small cycle.
	_, err = ParseTasksCfg(makeTasksCfg(map[string][]string{
		"a": []string{"b"},
		"b": []string{"a"},
	}))
	// Can't use a specific error message because map iteration order is non-deterministic.
	assert.Error(t, err)

	// Longer cycle.
	_, err = ParseTasksCfg(makeTasksCfg(map[string][]string{
		"a": []string{"b"},
		"b": []string{"c"},
		"c": []string{"d"},
		"d": []string{"e"},
		"e": []string{"f"},
		"f": []string{"g"},
		"g": []string{"h"},
		"h": []string{"i"},
		"i": []string{"j"},
		"j": []string{"a"},
	}))
	assert.Error(t, err)

	// No false positive on a complex-ish graph.
	_, err = ParseTasksCfg(makeTasksCfg(map[string][]string{
		"a": []string{},
		"b": []string{"a"},
		"c": []string{"a"},
		"d": []string{"b"},
		"e": []string{"b"},
		"f": []string{"c"},
		"g": []string{"d", "e", "f"},
	}))
	assert.NoError(t, err)
}
