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
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingApp struct{ envs []*AppEnv }

func (r *recordingApp) Step(env *AppEnv) (bool, error) {
	r.envs = append(r.envs, env)
	return true, nil
}

func appTestPump(t *testing.T, types ...string) (*Pump, []*User) {
	t.Helper()
	ka := &UTxOKey{Address: []byte{0x60, 0x0a}}
	kb := &UTxOKey{Address: []byte{0x60, 0x0b}}
	w := NewWallet()
	w.Add(
		UTxO{TxHash: strings.Repeat("aa", 32), Amount: 1, SigningKey: ka},
		UTxO{TxHash: strings.Repeat("bb", 32), Amount: 1, SigningKey: kb},
	)
	p := NewPump(
		&Config{Types: types},
		w,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
		time.Now(),
	)
	p.users = usersFromWallet(w)
	return p, p.users
}

func TestEnabledTypesIncludesRegisteredApps(t *testing.T) {
	resetAppsForTest(t)
	RegisterApp("fake_once", &fakeApp{})
	got := enabledTypes([]string{"payment", "fake_once", "nope"}, 0, false)
	assert.Equal(t, []string{"payment", "fake_once"}, got)
}

func TestRunBatchDispatchesRegisteredAppWithUser(t *testing.T) {
	resetAppsForTest(t)
	app := &recordingApp{}
	RegisterApp("rec", app)
	p, users := appTestPump(t, "rec")

	submitted := p.runBatch(context.Background(), nil, 5)

	assert.Equal(t, 5, submitted)
	require.Len(t, app.envs, 5)
	for _, env := range app.envs {
		assert.Contains(t, users, env.User)
		assert.Equal(t, users, env.Users)
		assert.Same(t, p.wallet, env.Wallet)
	}
}

func TestRunAppWithoutUsersDoesNothing(t *testing.T) {
	resetAppsForTest(t)
	app := &recordingApp{}
	p, _ := appTestPump(t, "rec")
	p.users = nil
	assert.False(t, p.runApp(nil, 1, "rec", app))
	assert.Empty(t, app.envs)
}
