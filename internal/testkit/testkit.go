// Package testkit reads the recorded answers in testdata and gives tests a Postgres schema of their own
package testkit

import (
	"context"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"tokens/internal/store"
)

// EnvDSN names a database where the store tests may create and drop their own schemas
const EnvDSN = "TOKENS_TEST_PG"

// Root is the repository root, found from the working directory of the test
func Root(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test")
		}
		dir = parent
	}
}

// Read returns a file under testdata
func Read(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(Root(t), "testdata", path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Result decodes the hex result of a recorded JSON-RPC answer
func Result(t testing.TB, path string) []byte {
	t.Helper()
	var env struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(Read(t, path), &env); err != nil {
		t.Fatal(err)
	}
	out, err := hex.DecodeString(strings.TrimPrefix(env.Result, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// CallData decodes the eth_call data of a recorded JSON-RPC request
func CallData(t testing.TB, path string) []byte {
	t.Helper()
	var req struct {
		Params []jsontext.Value `json:"params"`
	}
	if err := json.Unmarshal(Read(t, path), &req); err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(req.Params[0], &msg); err != nil {
		t.Fatal(err)
	}
	out, err := hex.DecodeString(strings.TrimPrefix(msg.Data, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Header reads a recorded eth_getBlockByNumber answer as number and time
func Header(t testing.TB, path string) (uint64, time.Time) {
	t.Helper()
	var env struct {
		Result struct {
			Number    string `json:"number"`
			Timestamp string `json:"timestamp"`
		} `json:"result"`
	}
	if err := json.Unmarshal(Read(t, path), &env); err != nil {
		t.Fatal(err)
	}
	n, err1 := strconv.ParseUint(strings.TrimPrefix(env.Result.Number, "0x"), 16, 64)
	ts, err2 := strconv.ParseInt(strings.TrimPrefix(env.Result.Timestamp, "0x"), 16, 64)
	if err1 != nil || err2 != nil {
		t.Fatal("header is malformed")
	}
	return n, time.Unix(ts, 0).UTC()
}

type DB struct {
	Store  *store.Store
	Schema string
}

// NewDB skips unless TOKENS_TEST_PG is set, and makes a migrated schema that is dropped after the test
func NewDB(t testing.TB) *DB {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skip(EnvDSN + " is not set")
	}
	d := &DB{Schema: "t_" + strconv.FormatUint(rand.Uint64(), 16)}
	st, err := store.Connect(t.Context(), dsn, d.Schema)
	if err != nil {
		t.Fatal(err)
	}
	d.Store = st
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := st.Pool.Exec(ctx, "drop schema if exists "+pgx.Identifier{d.Schema}.Sanitize()+" cascade"); err != nil {
			t.Errorf("drop schema %s: %v", d.Schema, err)
		}
		st.Close()
	})
	if _, err := st.Pool.Exec(t.Context(), "create schema "+pgx.Identifier{d.Schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return d
}
