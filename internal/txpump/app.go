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
	"fmt"
	"log/slog"
	"sort"
	"sync"
)

// coreTypes are the built-in transaction types dispatched by the pump
// directly. Apps may not reuse these names.
var coreTypes = map[string]bool{
	"payment":    true,
	"delegation": true,
	"governance": true,
	"plutus":     true,
}

var (
	appsMu sync.RWMutex
	apps   = map[string]App{}
)

// AppEnv is what the pump hands an App for one step.
type AppEnv struct {
	// User is the identity acting in this step; Users includes it.
	User   *User
	Users  []*User
	Wallet *Wallet
	Cfg    *Config
	Slot   uint64
	Logger *slog.Logger
	// Submit sends txBytes to the node and records the attempt in the
	// transaction log under txType. A nil error means the node accepted it.
	Submit func(txType, txID string, txBytes []byte) error
}

// App is a pluggable workload selectable through TXPUMP_TYPES.
type App interface {
	// Step attempts one action for env.User. submitted is false, with a nil
	// error, when nothing was attempted (no funds, no peer) or the node
	// rejected the transaction; an error means the app itself failed.
	Step(env *AppEnv) (submitted bool, err error)
}

// RegisterApp makes an app selectable by name. It panics on a bad name
// because registration happens at init time, and a shadowed core type would
// silently change what a run exercises.
func RegisterApp(name string, app App) {
	if name == "" || app == nil {
		panic("txpump: RegisterApp requires a name and an app")
	}
	if coreTypes[name] {
		panic(fmt.Sprintf("txpump: %q is a core transaction type", name))
	}
	appsMu.Lock()
	defer appsMu.Unlock()
	if _, dup := apps[name]; dup {
		panic(fmt.Sprintf("txpump: app %q already registered", name))
	}
	apps[name] = app
}

func lookupApp(name string) (App, bool) {
	appsMu.RLock()
	defer appsMu.RUnlock()
	a, ok := apps[name]
	return a, ok
}

func registeredAppNames() []string {
	appsMu.RLock()
	defer appsMu.RUnlock()
	names := make([]string, 0, len(apps))
	for n := range apps {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
