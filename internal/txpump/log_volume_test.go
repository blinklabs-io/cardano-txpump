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
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// At the default level a batch costs one log line however many transactions
// it submits or has refused, and the per-transaction detail stays available
// at debug level and in the transaction log.
func TestBatchLogsOneInfoLineNotOnePerTransaction(t *testing.T) {
	var buf bytes.Buffer
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
		slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})),
		nil,
		time.Now().Add(-time.Second),
	)

	pump.runBatch(context.Background(), &NodeClient{addr: "dead"}, 20)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 1, buf.String())
	require.Contains(t, lines[0], `"msg":"batch"`)
	require.Contains(t, lines[0], `"attempted"`)
}

func TestParseLogLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"":      slog.LevelInfo,
		"info":  slog.LevelInfo,
		"DEBUG": slog.LevelDebug,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	} {
		got, err := ParseLogLevel(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	_, err := ParseLogLevel("loud")
	require.Error(t, err)
}
