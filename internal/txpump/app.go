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
	"reflect"
	"sort"
	"strings"
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

// gatedApp is implemented by apps that cannot run with every wallet, such as
// one needing a second user. A type that can never submit must not count as
// enabled, or the startup deadline stays armed and the pump exits on the
// readiness timeout instead of idling.
type gatedApp interface {
	Enabled(users int) bool
}

func appEnabled(app App, users int) bool {
	g, ok := app.(gatedApp)
	return !ok || g.Enabled(users)
}

// RegisterApp makes an app selectable by name. It panics on a bad name
// because registration happens at init time, and a shadowed core type would
// silently change what a run exercises.
func RegisterApp(name string, app App) {
	if name == "" || isNilApp(app) {
		panic("txpump: RegisterApp requires a name and an app")
	}
	// TXPUMP_TYPES splits on commas and trims each item, so a name with a
	// comma or surrounding space could never be selected, and a padded core
	// name would select the built-in instead.
	if strings.TrimSpace(name) != name || strings.ContainsAny(name, ", \t\r\n") {
		panic(fmt.Sprintf("txpump: app name %q cannot be selected by TXPUMP_TYPES", name))
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

// isNilApp reports a nil interface or one holding a nil pointer, map, slice,
// function or channel, whose methods would panic when the pump called them.
func isNilApp(app App) bool {
	if app == nil {
		return true
	}
	v := reflect.ValueOf(app)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan,
		reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

func lookupApp(name string) (App, bool) {
	appsMu.RLock()
	defer appsMu.RUnlock()
	a, ok := apps[name]
	if !ok || a == nil {
		return nil, false
	}
	return a, true
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
