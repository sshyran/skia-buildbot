// Convenience utilities for testing.
package testutils

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"reflect"
	"runtime"
	"testing"

	"github.com/davecgh/go-spew/spew"
	assert "github.com/stretchr/testify/require"
)

// SkipIfShort causes the test to be skipped when running with -short.
func SkipIfShort(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping test with -short")
	}
}

// AssertDeepEqual fails the test if the two objects do not pass reflect.DeepEqual.
func AssertDeepEqual(t *testing.T, a, b interface{}) {
	if !reflect.DeepEqual(a, b) {
		assert.FailNow(t, fmt.Sprintf("Objects do not match: \na:\n%s\n\nb:\n%s\n", spew.Sprint(a), spew.Sprint(b)))
	}
}

// TestDataDir returns the path to the caller's testdata directory, which
// is assumed to be "<path to caller dir>/testdata".
func TestDataDir() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("Could not find test data dir: runtime.Caller() failed.")
	}
	for skip := 0; ; skip++ {
		_, file, _, ok := runtime.Caller(skip)
		if !ok {
			return "", fmt.Errorf("Could not find test data dir: runtime.Caller() failed.")
		}
		if file != thisFile {
			return path.Join(path.Dir(file), "testdata"), nil
		}
	}
}

func readFile(filename string) (io.Reader, error) {
	dir, err := TestDataDir()
	if err != nil {
		return nil, fmt.Errorf("Could not read %s: %v", filename, err)
	}
	f, err := os.Open(path.Join(dir, filename))
	if err != nil {
		return nil, fmt.Errorf("Could not read %s: %v", filename, err)
	}
	return f, nil
}

// ReadFile reads a file from the caller's testdata directory.
func ReadFile(filename string) (string, error) {
	f, err := readFile(filename)
	if err != nil {
		return "", err
	}
	b, err := ioutil.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("Could not read %s: %v", filename, err)
	}
	return string(b), nil
}

// MustReadFile reads a file from the caller's testdata directory and panics on
// error.
func MustReadFile(filename string) string {
	s, err := ReadFile(filename)
	if err != nil {
		panic(err)
	}
	return s
}

// ReadJsonFile reads a JSON file from the caller's testdata directory into the
// given interface.
func ReadJsonFile(filename string, dest interface{}) error {
	f, err := readFile(filename)
	if err != nil {
		return err
	}
	return json.NewDecoder(f).Decode(dest)
}

// MustReadJsonFile reads a JSON file from the caller's testdata directory into
// the given interface and panics on error.
func MustReadJsonFile(filename string, dest interface{}) {
	if err := ReadJsonFile(filename, dest); err != nil {
		panic(err)
	}
}

// CloseInTest takes an ioutil.Closer and Closes it, reporting any error.
func CloseInTest(t assert.TestingT, c io.Closer) {
	if err := c.Close(); err != nil {
		t.Errorf("Failed to Close(): %v", err)
	}
}

// AssertCloses takes an ioutil.Closer and asserts that it closes.
func AssertCloses(t assert.TestingT, c io.Closer) {
	assert.NoError(t, c.Close())
}

// Remove attempts to remove the given file and asserts that no error is returned.
func Remove(t assert.TestingT, fp string) {
	assert.NoError(t, os.Remove(fp))
}

// RemoveAll attempts to remove the given directory and asserts that no error is returned.
func RemoveAll(t assert.TestingT, fp string) {
	assert.NoError(t, os.RemoveAll(fp))
}
