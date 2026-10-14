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
	"testing"

	"github.com/stretchr/testify/assert"
)

type fakeApp struct{ calls int }

func (f *fakeApp) Step(*AppEnv) (bool, error) { f.calls++; return true, nil }

// resetAppsForTest swaps in an empty registry for the duration of a test.
func resetAppsForTest(t *testing.T) {
	t.Helper()
	appsMu.Lock()
	saved := apps
	apps = map[string]App{}
	appsMu.Unlock()
	t.Cleanup(func() {
		appsMu.Lock()
		apps = saved
		appsMu.Unlock()
	})
}

func TestRegisterAppRejectsBadNames(t *testing.T) {
	resetAppsForTest(t)
	for _, name := range []string{"", "payment", "delegation", "governance", "plutus"} {
		assert.Panics(t, func() { RegisterApp(name, &fakeApp{}) }, "name %q", name)
	}
	assert.Panics(t, func() { RegisterApp("nil_app", nil) })
}

func TestRegisterAppRejectsDuplicate(t *testing.T) {
	resetAppsForTest(t)
	RegisterApp("fake_once", &fakeApp{})
	assert.Panics(t, func() { RegisterApp("fake_once", &fakeApp{}) })
}

func TestLookupAndNames(t *testing.T) {
	resetAppsForTest(t)
	RegisterApp("zeta", &fakeApp{})
	RegisterApp("alpha", &fakeApp{})
	_, ok := lookupApp("alpha")
	assert.True(t, ok)
	_, ok = lookupApp("missing")
	assert.False(t, ok)
	assert.Equal(t, []string{"alpha", "zeta"}, registeredAppNames())
}
