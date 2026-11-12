package localcell_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestLifecycleFixtureMemoizerRetriesFailure(t *testing.T) {
	var memo lifecycleFixtureMemoizer
	want := []byte("synthetic fixture")
	failure := errors.New("synthetic fixture build failed")
	builds := 0
	build := func() ([]byte, error) {
		builds++
		if builds == 1 {
			return nil, failure
		}
		return want, nil
	}
	if data, err := memo.image(build); !errors.Is(err, failure) || data != nil {
		t.Fatal("fixture memoizer did not return its initial build failure")
	}
	for range 2 {
		if data, err := memo.image(build); err != nil || !bytes.Equal(data, want) {
			t.Fatal("fixture memoizer did not recover after a failed build")
		}
	}
	if builds != 2 {
		t.Fatal("fixture memoizer repeated a successful build")
	}
}

func TestLifecycleFixtureMemoizerBuildsOnce(t *testing.T) {
	var memo lifecycleFixtureMemoizer
	var builds atomic.Int32
	want := []byte("synthetic fixture")
	build := func() ([]byte, error) {
		builds.Add(1)
		return want, nil
	}
	const callers = 16
	results := make(chan bool, callers)
	start := make(chan struct{})
	var done sync.WaitGroup
	for range callers {
		done.Go(func() {
			<-start
			data, err := memo.image(build)
			results <- err == nil && bytes.Equal(data, want)
		})
	}
	close(start)
	done.Wait()
	close(results)
	for ok := range results {
		if !ok {
			t.Fatal("fixture memoizer did not serve every caller")
		}
	}
	if builds.Load() != 1 {
		t.Fatal("fixture memoizer built more than once")
	}
}

func TestLifecycleFixtureMemoizerCopiesBytes(t *testing.T) {
	var memo lifecycleFixtureMemoizer
	original := []byte("synthetic fixture")
	want := bytes.Clone(original)
	build := func() ([]byte, error) { return original, nil }
	first, err := memo.image(build)
	if err != nil || !bytes.Equal(first, want) {
		t.Fatal("fixture memoizer did not return the built image")
	}
	first[0] = 'x'
	original[1] = 'x'
	second, err := memo.image(build)
	if err != nil || !bytes.Equal(second, want) {
		t.Fatal("fixture memoizer exposed its retained bytes")
	}
}

func TestLifecycleFixtureCopiesAreIsolated(t *testing.T) {
	first := buildLifecycleFixture(t)
	second := buildLifecycleFixture(t)
	firstPath := filepath.Join(first.PostgresDir, lifecycleExecutableName("fixture"))
	secondPath := filepath.Join(second.PostgresDir, lifecycleExecutableName("fixture"))
	firstInfo, firstErr := os.Stat(firstPath)
	secondInfo, secondErr := os.Stat(secondPath)
	if firstErr != nil || secondErr != nil {
		t.Fatal("fixture copy identity unavailable")
	}
	if os.SameFile(firstInfo, secondInfo) {
		t.Fatal("fixture callers shared an executable inode")
	}
	firstData, firstReadErr := os.ReadFile(firstPath)
	secondData, secondReadErr := os.ReadFile(secondPath)
	if firstReadErr != nil || secondReadErr != nil || len(firstData) == 0 || !bytes.Equal(firstData, secondData) {
		t.Fatal("fixture copies did not retain identical executable bytes")
	}
	for _, fixture := range []string{firstPath, secondPath} {
		fixtureInfo, err := os.Stat(fixture)
		if err != nil {
			t.Fatal("fixture link identity unavailable")
		}
		for _, name := range []string{"postgres", "initdb", "pg_ctl", "createdb", "psql", "witself-server", "witself-worker"} {
			linkedInfo, err := os.Stat(filepath.Join(filepath.Dir(fixture), lifecycleExecutableName(name)))
			if err != nil || !os.SameFile(fixtureInfo, linkedInfo) {
				t.Fatal("fixture tool did not share its caller executable inode")
			}
		}
	}
}
