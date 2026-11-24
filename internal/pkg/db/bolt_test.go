package db

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBoltDB(t *testing.T) {
	dir, err := ioutil.TempDir("", "musig-bolt-test-")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	testFile := filepath.Join(dir, "test.db")
	database, err := NewBoltDB(testFile)
	require.NoError(t, err)

	testDatabase(t, database)

	err = os.Remove(testFile)
	require.NoError(t, err)
}
