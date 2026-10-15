// Copyright 2026 Blink Labs Software
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package txpump

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blinklabs-io/gouroboros/protocol/localtxsubmission"
	"github.com/stretchr/testify/require"
)

func reconnectPump(cfg *Config) *Pump {
	wallet := NewWallet()
	wallet.Add(UTxO{TxHash: "funding", Amount: 5_000_000})
	cfg.TxCountMin, cfg.TxCountMax = 1, 1
	return NewPump(
		cfg,
		wallet,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
		time.Now().Add(-time.Second),
	)
}

// A node that stays down is retried forever when no startup timeout is
// set: the pump neither returns an error nor gives up, and it submits as
// soon as the node answers again.
func TestRunRetriesAnUnreachableNodeUntilItReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pump := reconnectPump(&Config{StartupTimeout: 0})
	const downDials = 25
	dials := 0
	pump.dialPrimaryFn = func() (*NodeClient, error) {
		dials++
		if dials <= downDials {
			return nil, errors.New("connection refused")
		}
		return &NodeClient{addr: "primary"}, nil
	}
	pump.cooldownFn = func(context.Context) bool { return true }
	batches := 0
	pump.runBatchFn = func(context.Context, *NodeClient, int) int {
		batches++
		cancel()
		return 1
	}

	require.ErrorIs(t, pump.Run(ctx), context.Canceled)
	require.Equal(t, downDials+1, dials)
	require.Equal(t, 1, batches)
}

// Once the pump has submitted, losing the node costs only the batches
// attempted while it is gone.
func TestRunKeepsGoingAfterTheNodeDiesFollowingReadiness(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pump := reconnectPump(&Config{StartupTimeout: time.Minute})
	dials := 0
	pump.dialPrimaryFn = func() (*NodeClient, error) {
		dials++
		// The node is up for the first dial, down for the next ten, then
		// back.
		if dials >= 2 && dials <= 11 {
			return nil, errors.New("connection refused")
		}
		return &NodeClient{addr: "primary"}, nil
	}
	pump.cooldownFn = func(context.Context) bool { return true }
	batches := 0
	pump.runBatchFn = func(context.Context, *NodeClient, int) int {
		batches++
		if batches == 2 {
			cancel()
		}
		return 1
	}

	require.ErrorIs(t, pump.Run(ctx), context.Canceled)
	require.Equal(t, 2, batches)
	require.Equal(t, 12, dials)
}

// With the primary down, a batch runs against the configured backup.
func TestRunSubmitsThroughTheBackupWhilePrimaryIsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pump := reconnectPump(&Config{StartupTimeout: 0, FallbackAddr: "backup"})
	pump.dialPrimaryFn = func() (*NodeClient, error) {
		return nil, errors.New("connection refused")
	}
	pump.dialFallbackFn = func() (*NodeClient, error) {
		return &NodeClient{addr: "backup"}, nil
	}
	pump.cooldownFn = func(context.Context) bool { return true }
	var used string
	pump.runBatchFn = func(_ context.Context, c *NodeClient, _ int) int {
		used = c.Addr()
		cancel()
		return 1
	}

	require.ErrorIs(t, pump.Run(ctx), context.Canceled)
	require.Equal(t, "backup", used)
}

func TestSubmitFailureStatusSeparatesRejectionFromTransportFailure(t *testing.T) {
	rejected := fmt.Errorf(
		"node: %w", localtxsubmission.TransactionRejectedError{},
	)
	require.Equal(t, "rejected", submitFailureStatus(rejected))
	require.Equal(
		t,
		"error",
		submitFailureStatus(errors.New("protocol is shutting down")),
	)
}

func TestSubmitFailureMessageSeparatesRejectionFromTransportFailure(t *testing.T) {
	rejected := fmt.Errorf("node: %w", localtxsubmission.TransactionRejectedError{})
	require.Equal(t, "tx rejected", submitFailureMessage("tx", rejected))
	require.Equal(
		t,
		"plutus tx submit failed",
		submitFailureMessage("plutus tx", errors.New("protocol is shutting down")),
	)
}

// A connection that fails mid-batch ends the batch: every later submission
// would wait out its own timeout on the same dead connection.
func TestRunBatchStopsOnceTheConnectionFails(t *testing.T) {
	dir := t.TempDir()
	txlog, err := NewTxLogger(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = txlog.Close() })
	wallet := NewWallet()
	for i := range 20 {
		wallet.Add(UTxO{
			TxHash: fmt.Sprintf("%064x", i+1),
			Amount: 20_000_000,
		})
	}
	pump := NewPump(
		&Config{Types: []string{"payment"}, TxCountMin: 1, TxCountMax: 1},
		wallet,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		txlog,
		time.Now().Add(-time.Second),
	)

	pump.runBatch(context.Background(), &NodeClient{addr: "dead"}, 20)

	raw, err := os.ReadFile(filepath.Join(dir, "txpump.log"))
	require.NoError(t, err)
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	require.Len(t, lines, 1, "the batch must stop at the first transport failure")
	require.Contains(t, string(lines[0]), `"status":"error"`)
}

// A node that stays down is reported once when the outage starts and once
// when it ends; the retries in between do not each log an error.
func TestRunLogsAPrimaryOutageOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var buf bytes.Buffer
	pump := reconnectPump(&Config{StartupTimeout: 0})
	pump.logger = slog.New(slog.NewTextHandler(&buf, nil))
	const downDials = 25
	dials := 0
	pump.dialPrimaryFn = func() (*NodeClient, error) {
		dials++
		if dials <= downDials {
			return nil, errors.New("connection refused")
		}
		return &NodeClient{addr: "primary"}, nil
	}
	pump.cooldownFn = func(context.Context) bool { return true }
	pump.runBatchFn = func(context.Context, *NodeClient, int) int {
		cancel()
		return 1
	}

	require.ErrorIs(t, pump.Run(ctx), context.Canceled)
	out := buf.String()
	require.Equal(
		t, 1,
		strings.Count(out, "level=ERROR msg=\"failed to connect to primary node"),
	)
	require.Equal(t, 1, strings.Count(out, "primary node connection restored"))
}
