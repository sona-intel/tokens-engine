// Package testkit reads the recorded answers in testdata and gives tests a Postgres schema of their own
package testkit

import (
	"context"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
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

// LogsPage turns a recorded eth_getLogs answer into the HyperSync answer of the same query, ending where the request ended
func LogsPage(t testing.TB, path string) []byte {
	t.Helper()
	var resp struct {
		Result []struct {
			Address        string   `json:"address"`
			Topics         []string `json:"topics"`
			Data           string   `json:"data"`
			BlockNumber    string   `json:"blockNumber"`
			LogIndex       string   `json:"logIndex"`
			TxHash         string   `json:"transactionHash"`
			BlockTimestamp string   `json:"blockTimestamp"`
		} `json:"result"`
	}
	var req struct {
		Params []struct {
			ToBlock string `json:"toBlock"`
		} `json:"params"`
	}
	if err := errors.Join(json.Unmarshal(Read(t, path), &resp), json.Unmarshal(Read(t, strings.TrimSuffix(path, ".json")+".request.json"), &req)); err != nil {
		t.Fatal(err)
	}
	if len(req.Params) != 1 || len(resp.Result) == 0 {
		t.Fatalf("%s is not one eth_getLogs answer", path)
	}
	to, err := strconv.ParseUint(strings.TrimPrefix(req.Params[0].ToBlock, "0x"), 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	type hsLog struct {
		BlockNumber string  `json:"block_number"`
		LogIndex    string  `json:"log_index"`
		TxHash      string  `json:"transaction_hash"`
		Address     string  `json:"address"`
		Data        string  `json:"data"`
		Topic0      *string `json:"topic0,omitzero"`
		Topic1      *string `json:"topic1,omitzero"`
		Topic2      *string `json:"topic2,omitzero"`
	}
	type hsBlock struct {
		Number    string `json:"number"`
		Timestamp string `json:"timestamp"`
	}
	var (
		logs   []hsLog
		blocks []hsBlock
		seen   = map[string]bool{}
	)
	for _, l := range resp.Result {
		out := hsLog{BlockNumber: l.BlockNumber, LogIndex: l.LogIndex, TxHash: l.TxHash, Address: l.Address, Data: l.Data}
		topics := []**string{&out.Topic0, &out.Topic1, &out.Topic2}
		for i := range min(len(l.Topics), len(topics)) {
			*topics[i] = &l.Topics[i]
		}
		logs = append(logs, out)
		if !seen[l.BlockNumber] {
			seen[l.BlockNumber] = true
			blocks = append(blocks, hsBlock{Number: l.BlockNumber, Timestamp: l.BlockTimestamp})
		}
	}
	type hsData struct {
		Logs   []hsLog   `json:"logs"`
		Blocks []hsBlock `json:"blocks"`
	}
	body, err := json.Marshal(struct {
		Data          []hsData `json:"data"`
		ArchiveHeight uint64   `json:"archive_height"`
		NextBlock     uint64   `json:"next_block"`
	}{Data: []hsData{{Logs: logs, Blocks: blocks}}, ArchiveHeight: to + 1, NextBlock: to + 1})
	if err != nil {
		t.Fatal(err)
	}
	return body
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
