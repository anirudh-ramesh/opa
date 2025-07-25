// Copyright 2018 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

// nolint: goconst // string duplication is for test readability.
package discovery

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-policy-agent/opa/v1/ast"
	bundleApi "github.com/open-policy-agent/opa/v1/bundle"
	"github.com/open-policy-agent/opa/v1/download"
	"github.com/open-policy-agent/opa/v1/logging/test"
	"github.com/open-policy-agent/opa/v1/metrics"
	"github.com/open-policy-agent/opa/v1/plugins"
	bundlePlugin "github.com/open-policy-agent/opa/v1/plugins/bundle"
	"github.com/open-policy-agent/opa/v1/plugins/logs"
	"github.com/open-policy-agent/opa/v1/plugins/status"
	"github.com/open-policy-agent/opa/v1/rego"
	"github.com/open-policy-agent/opa/v1/server"
	"github.com/open-policy-agent/opa/v1/storage"
	inmem "github.com/open-policy-agent/opa/v1/storage/inmem/test"
	"github.com/open-policy-agent/opa/v1/topdown/cache"
	"github.com/open-policy-agent/opa/v1/util"
	"github.com/open-policy-agent/opa/v1/version"
)

const (
	snapshotBundleSize = 1024
)

func TestMain(m *testing.M) {
	if version.Version == "" {
		version.Version = "unit-test"
	}
	os.Exit(m.Run())
}

func TestEvaluateBundle(t *testing.T) {

	sampleModule := `
		package foo.bar
		import rego.v1

		bundle = {
			"name": rt.name,
			"service": "example"
		} if {
			rt := opa.runtime()
		}
	`

	b := &bundleApi.Bundle{
		Manifest: bundleApi.Manifest{
			Revision: "quickbrownfaux",
		},
		Data: map[string]any{
			"foo": map[string]any{
				"bar": map[string]any{
					"status": map[string]any{},
				},
			},
		},
		Modules: []bundleApi.ModuleFile{
			{
				Path:   `/example.rego`,
				Raw:    []byte(sampleModule),
				Parsed: ast.MustParseModule(sampleModule),
			},
		},
	}

	info := ast.MustParseTerm(`{"name": "test/bundle1"}`)

	config, err := evaluateBundle(context.Background(), "test-id", info, b, "data.foo.bar")
	if err != nil {
		t.Fatal(err)
	}

	if config.Bundle == nil {
		t.Fatal("Expected a bundle configuration")
	}

	var parsedConfig bundlePlugin.Config

	if err := util.Unmarshal(config.Bundle, &parsedConfig); err != nil {
		t.Fatal("Unexpected error:", err)
	}

	expectedBundleConfig := bundlePlugin.Config{
		Name:    "test/bundle1",
		Service: "example",
	}

	if !reflect.DeepEqual(expectedBundleConfig, parsedConfig) {
		t.Fatalf("Expected bundle config %v, but got %v", expectedBundleConfig, parsedConfig)
	}

}

func TestProcessBundle(t *testing.T) {

	ctx := context.Background()

	manager, err := plugins.New([]byte(`{
		"services": {
			"default": {
				"url": "http://localhost:8181"
			}
		},
		"discovery": {"name": "config"}
	}`), "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	initialBundle := makeDataBundle(1, `
		{
			"config": {
				"bundle": {"name": "test1"},
				"status": {},
				"decision_logs": {}
			}
		}
	`)

	disco, err := New(manager)
	if err != nil {
		t.Fatal(err)
	}

	ps, err := disco.processBundle(ctx, initialBundle)
	if err != nil {
		t.Fatal(err)
	}

	if len(ps.Start) != 3 || len(ps.Reconfig) != 0 {
		t.Fatalf("Expected exactly three start events but got %v", ps)
	}

	updatedBundle := makeDataBundle(1, `
		{
			"config": {
				"bundle": {"name": "test2"},
				"status": {"partition_name": "foo"},
				"decision_logs": {"partition_name": "bar"}
			}
		}
	`)

	ps, err = disco.processBundle(ctx, updatedBundle)
	if err != nil {
		t.Fatal(err)
	}

	if len(ps.Start) != 0 || len(ps.Reconfig) != 3 {
		t.Fatalf("Expected exactly three start events but got %v", ps)
	}

	updatedBundle = makeDataBundle(2, `
		{
			"config": {
				"bundle": {"service": "missing service name", "name": "test2"}
			}
		}
	`)

	_, err = disco.processBundle(ctx, updatedBundle)
	if err == nil {
		t.Fatal("Expected error but got success")
	}

}

func TestEnvVarSubstitution(t *testing.T) {

	ctx := context.Background()

	manager, err := plugins.New([]byte(`{
		"services": {
			"default": {
				"url": "http://localhost:8181"
			}
		},
		"discovery": {"name": "config"}
	}`), "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("ENV1", "test1")
	initialBundle := makeDataBundle(1, `
		{
			"config": {
				"bundle": {"name": "${ENV1}"},
				"status": {},
				"decision_logs": {}
			}
		}
	`)

	disco, err := New(manager)
	if err != nil {
		t.Fatal(err)
	}

	ps, err := disco.processBundle(ctx, initialBundle)
	if err != nil {
		t.Fatal(err)
	}

	if len(ps.Start) != 3 || len(ps.Reconfig) != 0 {
		t.Fatalf("Expected exactly three start events but got %v", ps)
	}

	actualConfig, err := manager.Config.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	assertConfig(t, actualConfig, fmt.Sprintf(`{
	"bundle": {
		"name": "test1"
	},
	"decision_logs": {},
	"default_authorization_decision": "/system/authz/allow",
	"default_decision": "/system/main",
	"discovery": {
		"name": "config"
	},
	"labels": {
		"id": "test-id",
		"version": %v
	},
	"status": {}
}`, version.Version))
}

func TestProcessBundleV1Compatible(t *testing.T) {
	ctx := context.Background()
	popts := ast.ParserOptions{RegoVersion: ast.RegoV1}

	manager, err := plugins.New([]byte(`{
		"services": {
			"default": {
				"url": "http://localhost:8181"
			}
		},
		"discovery": {"name": "config"}
	}`), "test-id",
		inmem.New(),
		plugins.WithParserOptions(popts))
	if err != nil {
		t.Fatal(err)
	}

	initialBundle := makeModuleBundle(1, `package config
bundle.name := "test1"
status := {}
decision_logs := {} if { 3 == 3 }
`, popts)

	disco, err := New(manager)
	if err != nil {
		t.Fatal(err)
	}

	ps, err := disco.processBundle(ctx, initialBundle)
	if err != nil {
		t.Fatal(err)
	}

	if len(ps.Start) != 3 || len(ps.Reconfig) != 0 {
		t.Fatalf("Expected exactly three start events but got %v", ps)
	}

	actualConfig, err := manager.Config.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	assertConfig(t, actualConfig, fmt.Sprintf(`{
	"bundle": {
		"name": "test1"
	},
	"decision_logs": {},
	"default_authorization_decision": "/system/authz/allow",
	"default_decision": "/system/main",
	"discovery": {
		"name": "config"
	},
	"labels": {
		"id": "test-id",
		"version": %v
	},
	"status": {}
}`, version.Version))

	// The bundle is parsed outside the discovery service, but is still compiled by it during processing.
	// As such, it is impossible to pass it a module that doesn't pass the parsing step.
	// We first pass it a valid v1.0 policy ...
	updatedBundle := makeModuleBundle(1, `package config
bundle.name := "test2" if { 1 == 1 }
status.partition_name := "foo" if { 2 == 2 }
decision_logs.partition_name := "bar" if { 3 == 3 }
`, popts)

	ps, err = disco.processBundle(ctx, updatedBundle)
	if err != nil {
		t.Fatal(err)
	}

	if len(ps.Start) != 0 || len(ps.Reconfig) != 3 {
		t.Fatalf("Expected exactly three start events but got %v", ps)
	}

	actualConfig, err = manager.Config.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	assertConfig(t, actualConfig, fmt.Sprintf(`{
	"bundle": {
		"name": "test2"
	},
	"decision_logs": {
		"partition_name": "bar"
	},
	"default_authorization_decision": "/system/authz/allow",
	"default_decision": "/system/main",
	"discovery": {
		"name": "config"
	},
	"labels": {
		"id": "test-id",
		"version": %v
	},
	"status": {
		"partition_name": "foo"
	}
}`, version.Version))

	// ... and then an invalid v1.0 policy, where we expect the compiler to complain about shadowed imports (which passes the parsing step).
	updatedBundle = makeModuleBundle(1, `package config
import data.foo
import data.bar as foo

bundle.name := "test2" if { 1 == 1 }
status.partition_name := "foo" if { 2 == 2 }
decision_logs.partition_name := "bar" if { 3 == 3 }
`, popts)

	_, err = disco.processBundle(ctx, updatedBundle)
	if err == nil {
		t.Fatal("Expected error but got none")
	}
	expErr := `rego_compile_error: import must not shadow import data.foo`
	if !strings.Contains(err.Error(), expErr) {
		t.Fatalf("Expected error:\n\n%v\n\nbut got:\n\n%v", expErr, err)
	}
}

func TestProcessBundleWithActiveConfig(t *testing.T) {

	ctx := context.Background()

	manager, err := plugins.New([]byte(`{
		"labels": {"x": "y"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999",
				"credentials": {"bearer": {"token": "test"}}
			}
		},
		"keys": {
			"local_key": {
				"private_key": "local"
			}
		},
		"discovery": {"name": "config"},
	}`), "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	initialBundle := makeDataBundle(1, `
		{
			"config": {
				"services": {
					"acmecorp": {
						"url": "https://example.com/control-plane-api/v1",
						"credentials": {"bearer": {"token": "test-acmecorp"}}
					}
				},
				"bundles": {"test-bundle": {"service": "localhost"}},
				"status": {"partition_name": "foo"},
				"decision_logs": {"partition_name": "bar"},
				"default_decision": "bar/baz",
				"default_authorization_decision": "baz/qux",
				"keys": {
					"global_key": {
						"scope": "read",
						"key": "secret"
					}
				}
			}
		}
	`)

	disco, err := New(manager)
	if err != nil {
		t.Fatal(err)
	}

	_, err = disco.processBundle(ctx, initialBundle)
	if err != nil {
		t.Fatal(err)
	}

	actual, err := manager.Config.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}

	expectedConfig := fmt.Sprintf(`{
		"services": {
			"acmecorp": {
				"url": "https://example.com/control-plane-api/v1"
			}
		},
		"labels": {
			"id": "test-id",
			"version": %v,
			"x": "y"
		},
		"keys": {
			"global_key": {
				"scope": "read"
			}
		},
		"decision_logs": {
			"partition_name": "bar"
		},
		"status": {
			"partition_name": "foo"
		},
		"bundles": {
			"test-bundle": {
				"service": "localhost"
			}
		},
		"default_authorization_decision": "baz/qux",
		"default_decision": "bar/baz",
		"discovery": {"name": "config"}
	}`, version.Version)

	assertConfig(t, actual, expectedConfig)

	initialBundle = makeDataBundle(2, `
		{
			"config": {
				"services": {
					"opa.example.com": {
						"url": "https://opa.example.com",
						"credentials": {"bearer": {"token": "test-opa"}}
					}
				},
				"bundles": {"test-bundle-2": {"service": "opa.example.com"}},
				"decision_logs": {},
				"keys": {
					"global_key_2": {
						"scope": "write",
						"key": "secret_2"
					}
				}
			}
		}
	`)

	_, err = disco.processBundle(ctx, initialBundle)
	if err != nil {
		t.Fatal(err)
	}

	actual, err = manager.Config.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}

	expectedConfig2 := fmt.Sprintf(`{
		"services": {
			"opa.example.com": {
				"url": "https://opa.example.com"
			}
		},
		"labels": {
			"id": "test-id",
			"version": %v,
			"x": "y"
		},
		"keys": {
			"global_key_2": {
				"scope": "write"
			}
		},
		"decision_logs": {},
		"bundles": {
			"test-bundle-2": {
				"service": "opa.example.com"
			}
		},
		"default_authorization_decision": "/system/authz/allow",
		"default_decision": "/system/main",
		"discovery": {"name": "config"}
	}`, version.Version)

	assertConfig(t, actual, expectedConfig2)
}

func assertConfig(t *testing.T, actualConfig any, expectedConfig string) {
	t.Helper()

	var expected map[string]any
	if err := util.Unmarshal([]byte(expectedConfig), &expected); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(actualConfig, expected) {
		t.Fatalf("expected config:\n\n%v\n\ngot:\n\n%v", expectedConfig, actualConfig)
	}
}

type testFactory struct {
	p *reconfigureTestPlugin
}

func (testFactory) Validate(*plugins.Manager, []byte) (any, error) {
	return nil, nil
}

func (f testFactory) New(*plugins.Manager, any) plugins.Plugin {
	return f.p
}

type reconfigureTestPlugin struct {
	counts map[string]int
}

func (r *reconfigureTestPlugin) Start(context.Context) error {
	r.counts["start"]++
	return nil
}

func (*reconfigureTestPlugin) Stop(context.Context) {
}

func (r *reconfigureTestPlugin) Reconfigure(_ context.Context, _ any) {
	r.counts["reconfig"]++
}

func TestStartWithBundlePersistence(t *testing.T) {
	dir := t.TempDir()

	initialBundle := makeDataBundle(1, `
		{
			"config": {
				"labels": {"x": "label value changed"},
				"default_decision": "bar/baz",
				"default_authorization_decision": "baz/qux",
				"plugins": {
					"test_plugin": {"a": "b"}
				}
			}
		}
	`)

	initialBundle.Manifest.Init()

	var buf bytes.Buffer
	if err := bundleApi.NewWriter(&buf).Write(*initialBundle); err != nil {
		t.Fatal("unexpected error:", err)
	}

	bundleDir := filepath.Join(dir, "bundles", "config")

	err := os.MkdirAll(bundleDir, os.ModePerm)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	if err := os.WriteFile(filepath.Join(bundleDir, "bundle.tar.gz"), buf.Bytes(), 0644); err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	manager, err := plugins.New([]byte(`{
		"labels": {"x": "y"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
		"discovery": {"name": "config", "persist": true},
	}`), "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	manager.Config.PersistenceDirectory = &dir

	testPlugin := &reconfigureTestPlugin{counts: map[string]int{}}
	testFactory := testFactory{p: testPlugin}

	disco, err := New(manager, Factories(map[string]plugins.Factory{"test_plugin": testFactory}))
	if err != nil {
		t.Fatal(err)
	}

	err = disco.Start(context.Background())
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	ensurePluginState(t, disco, plugins.StateOK)

	// verify the test plugin was registered on the manager
	if plugin := manager.Plugin("test_plugin"); plugin == nil {
		t.Fatalf("expected \"test_plugin\" to be regsitered with the plugin manager")
	}

	// verify the test plugin was started
	count, ok := testPlugin.counts["start"]
	if !ok {
		t.Fatal("expected test plugin to have start counter")
	}

	if count != 1 {
		t.Fatalf("expected test plugin to have a start count of 1 but got %v", count)
	}
}

func TestOneShotWithBundlePersistence(t *testing.T) {
	dir := t.TempDir()

	manager, err := plugins.New([]byte(`{
		"labels": {"x": "y"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
		"discovery": {"name": "config", "persist": true},
	}`), "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	testPlugin := &reconfigureTestPlugin{counts: map[string]int{}}
	testFactory := testFactory{p: testPlugin}

	disco, err := New(manager, Factories(map[string]plugins.Factory{"test_plugin": testFactory}))
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	disco.bundlePersistPath = filepath.Join(dir, ".opa")

	ensurePluginState(t, disco, plugins.StateNotReady)

	// simulate a bundle download error with no bundle on disk
	disco.oneShot(ctx, download.Update{Error: errors.New("unknown error")})

	if disco.status.Message == "" {
		t.Fatal("expected error but got none")
	}

	ensurePluginState(t, disco, plugins.StateNotReady)

	// download a bundle and persist to disk. Then verify the bundle persisted to disk
	initialBundle := makeDataBundle(1, `
		{
			"config": {
				"labels": {"x": "label value changed"},
				"default_decision": "bar/baz",
				"default_authorization_decision": "baz/qux",
				"plugins": {
					"test_plugin": {"a": "b"}
				}
			}
		}
	`)

	initialBundle.Manifest.Init()
	expBndl := initialBundle.Copy()

	var buf bytes.Buffer
	if err := bundleApi.NewWriter(&buf).Write(*initialBundle); err != nil {
		t.Fatal("unexpected error:", err)
	}

	disco.oneShot(ctx, download.Update{Bundle: initialBundle, ETag: "etag-1", Raw: &buf})

	ensurePluginState(t, disco, plugins.StateOK)

	result, err := disco.loadBundleFromDisk()
	if err != nil {
		t.Fatal("unexpected error:", err)
	}

	if !result.Equal(expBndl) {
		t.Fatalf("expected the downloaded bundle to be equal to the one loaded from disk: result=%v, exp=%v", result, expBndl)
	}

	// verify the test plugin was registered on the manager
	if plugin := manager.Plugin("test_plugin"); plugin == nil {
		t.Fatalf("expected \"test_plugin\" to be regsitered with the plugin manager")
	}

	// verify the test plugin was started
	count, ok := testPlugin.counts["start"]
	if !ok {
		t.Fatal("expected test plugin to have start counter")
	}

	if count != 1 {
		t.Fatalf("expected test plugin to have a start count of 1 but got %v", count)
	}
}

func TestLoadAndActivateBundleFromDisk(t *testing.T) {
	dir := t.TempDir()

	manager, err := plugins.New([]byte(`{
		"labels": {"x": "y"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
		"discovery": {"name": "config", "persist": true},
	}`), "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	testPlugin := &reconfigureTestPlugin{counts: map[string]int{}}
	testFactory := testFactory{p: testPlugin}

	disco, err := New(manager, Factories(map[string]plugins.Factory{"test_plugin": testFactory}))
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	disco.bundlePersistPath = filepath.Join(dir, ".opa")

	ensurePluginState(t, disco, plugins.StateNotReady)

	// persist a bundle to disk and then load it
	initialBundle := makeDataBundle(1, `
		{
			"config": {
				"labels": {"x": "label value changed"},
				"default_decision": "bar/baz",
				"default_authorization_decision": "baz/qux",
				"plugins": {
					"test_plugin": {"a": "b"}
				},
				"services": {
					"acmecorp": {
						"url": "http://localhost:8181"
					}
				},
				"bundles": {
					"authz": {
						"service": "acmecorp"
					}
				}
			}
		}
	`)

	initialBundle.Manifest.Init()

	var buf bytes.Buffer
	if err := bundleApi.NewWriter(&buf).Write(*initialBundle); err != nil {
		t.Fatal("unexpected error:", err)
	}

	err = disco.saveBundleToDisk(&buf)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	disco.loadAndActivateBundleFromDisk(ctx)

	ensurePluginState(t, disco, plugins.StateOK)

	// verify the test plugin was registered on the manager
	if plugin := manager.Plugin("test_plugin"); plugin == nil {
		t.Fatalf("expected \"test_plugin\" to be regsitered with the plugin manager")
	}

	// verify the test plugin was started
	count, ok := testPlugin.counts["start"]
	if !ok {
		t.Fatal("expected test plugin to have start counter")
	}

	if count != 1 {
		t.Fatalf("expected test plugin to have a start count of 1 but got %v", count)
	}

	// verify the bundle plugin was registered on the manager
	if plugin := bundlePlugin.Lookup(disco.manager); plugin == nil {
		t.Fatalf("expected bundle plugin to be regsitered with the plugin manager")
	}
}

func TestLoadAndActivateSignedBundleFromDisk(t *testing.T) {
	dir := t.TempDir()

	manager, err := plugins.New([]byte(`{
		"labels": {"x": "y"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
		"discovery": {"name": "config", "persist": true},
	}`), "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	testPlugin := &reconfigureTestPlugin{counts: map[string]int{}}
	testFactory := testFactory{p: testPlugin}

	disco, err := New(manager, Factories(map[string]plugins.Factory{"test_plugin": testFactory}))
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	disco.bundlePersistPath = filepath.Join(dir, ".opa")
	disco.config.Signing = bundleApi.NewVerificationConfig(map[string]*bundleApi.KeyConfig{"foo": {Key: "secret", Algorithm: "HS256"}}, "foo", "", nil)

	ensurePluginState(t, disco, plugins.StateNotReady)

	// persist a bundle to disk and then load it
	initialBundle := makeDataBundle(1, `
		{
			"config": {
				"labels": {"x": "label value changed"},
				"default_decision": "bar/baz",
				"default_authorization_decision": "baz/qux",
				"plugins": {
					"test_plugin": {"a": "b"}
				},
				"services": {
					"acmecorp": {
						"url": "http://localhost:8181"
					}
				},
				"bundles": {
					"authz": {
						"service": "acmecorp"
					}
				}
			}
		}
	`)

	initialBundle.Manifest.Init()

	if err := initialBundle.GenerateSignature(bundleApi.NewSigningConfig("secret", "HS256", ""), "foo", false); err != nil {
		t.Fatal("Unexpected error:", err)
	}

	var buf bytes.Buffer
	if err := bundleApi.NewWriter(&buf).Write(*initialBundle); err != nil {
		t.Fatal("unexpected error:", err)
	}

	err = disco.saveBundleToDisk(&buf)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	disco.loadAndActivateBundleFromDisk(ctx)

	ensurePluginState(t, disco, plugins.StateOK)

	// verify the test plugin was registered on the manager
	if plugin := manager.Plugin("test_plugin"); plugin == nil {
		t.Fatalf("expected \"test_plugin\" to be regsitered with the plugin manager")
	}

	// verify the test plugin was started
	count, ok := testPlugin.counts["start"]
	if !ok {
		t.Fatal("expected test plugin to have start counter")
	}

	if count != 1 {
		t.Fatalf("expected test plugin to have a start count of 1 but got %v", count)
	}

	// verify the bundle plugin was registered on the manager
	if plugin := bundlePlugin.Lookup(disco.manager); plugin == nil {
		t.Fatalf("expected bundle plugin to be regsitered with the plugin manager")
	}
}

func TestLoadAndActivateBundleFromDiskMaxAttempts(t *testing.T) {
	dir := t.TempDir()

	manager, err := plugins.New([]byte(`{
		"labels": {"x": "y"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
		"discovery": {"name": "config", "persist": true},
	}`), "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	testPlugin := &reconfigureTestPlugin{counts: map[string]int{}}
	testFactory := testFactory{p: testPlugin}

	disco, err := New(manager, Factories(map[string]plugins.Factory{"test_plugin": testFactory}))
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	disco.bundlePersistPath = filepath.Join(dir, ".opa")

	ensurePluginState(t, disco, plugins.StateNotReady)

	// persist a bundle to disk and then load it
	// this bundle should never activate as the service discovery depends on is modified
	initialBundle := makeDataBundle(1, `
		{
			"config": {
				"labels": {"x": "label value changed"},
				"default_decision": "bar/baz",
				"default_authorization_decision": "baz/qux",
				"plugins": {
					"test_plugin": {"a": "b"}
				},
				"services": {
					"localhost": {
						"url": "http://localhost:8181"
					}
				},
				"bundles": {
					"authz": {
						"service": "localhost"
					}
				}
			}
		}
	`)

	initialBundle.Manifest.Init()

	var buf bytes.Buffer
	if err := bundleApi.NewWriter(&buf).Write(*initialBundle); err != nil {
		t.Fatal("unexpected error:", err)
	}

	err = disco.saveBundleToDisk(&buf)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	disco.loadAndActivateBundleFromDisk(ctx)

	ensurePluginState(t, disco, plugins.StateNotReady)

	if len(manager.Plugins()) != 0 {
		t.Fatal("expected no plugins to be registered with the plugin manager")
	}
}

func TestLoadAndActivateBundleFromDiskV1Compatible(t *testing.T) {
	tests := []struct {
		note         string
		v1Compatible bool
		bundle       string
	}{
		{
			note: "v0.x",
			bundle: `package config
import future.keywords

labels.x := "label value changed"
default_decision := "bar/baz"
default_authorization_decision := "baz/qux"
plugins.test_plugin := v if {
	v := {"a": "b"}
}
services.acmecorp.url := v if {
	v := "http://localhost:8181"
}
bundles.authz.service := v if {
	v := "localhost"
}
`,
		},
		{
			note:         "v1.0",
			v1Compatible: true,
			// no future.keywords import
			bundle: `package config
labels.x := "label value changed"
default_decision := "bar/baz"
default_authorization_decision := "baz/qux"
plugins.test_plugin := v if {
	v := {"a": "b"}
}
services.acmecorp.url := v if {
	v := "http://localhost:8181"
}
bundles.authz.service := v if {
	v := "localhost"
}
`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.note, func(t *testing.T) {
			regoVersion := ast.RegoV0
			if tc.v1Compatible {
				regoVersion = ast.RegoV1
			}
			popts := ast.ParserOptions{RegoVersion: regoVersion}
			dir := t.TempDir()

			manager, err := plugins.New([]byte(`{
		"labels": {"x": "y"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
		"discovery": {"name": "config", "persist": true},
	}`), "test-id",
				inmem.New(),
				plugins.WithParserOptions(popts))
			if err != nil {
				t.Fatal(err)
			}

			testPlugin := &reconfigureTestPlugin{counts: map[string]int{}}
			testFactory := testFactory{p: testPlugin}

			disco, err := New(manager, Factories(map[string]plugins.Factory{"test_plugin": testFactory}))
			if err != nil {
				t.Fatal(err)
			}

			ctx := context.Background()

			disco.bundlePersistPath = filepath.Join(dir, ".opa")

			ensurePluginState(t, disco, plugins.StateNotReady)

			// persist a bundle to disk and then load it
			initialBundle := makeModuleBundle(1, tc.bundle, popts)

			initialBundle.Manifest.Init()

			var buf bytes.Buffer
			if err := bundleApi.NewWriter(&buf).Write(*initialBundle); err != nil {
				t.Fatal("unexpected error:", err)
			}

			err = disco.saveBundleToDisk(&buf)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}

			disco.loadAndActivateBundleFromDisk(ctx)

			ensurePluginState(t, disco, plugins.StateOK)

			// verify the test plugin was registered on the manager
			if plugin := manager.Plugin("test_plugin"); plugin == nil {
				t.Fatalf("expected \"test_plugin\" to be regsitered with the plugin manager")
			}

			// verify the test plugin was started
			count, ok := testPlugin.counts["start"]
			if !ok {
				t.Fatal("expected test plugin to have start counter")
			}

			if count != 1 {
				t.Fatalf("expected test plugin to have a start count of 1 but got %v", count)
			}

			// verify the bundle plugin was registered on the manager
			if plugin := bundlePlugin.Lookup(disco.manager); plugin == nil {
				t.Fatalf("expected bundle plugin to be regsitered with the plugin manager")
			}
		})
	}
}

func TestLoadAndActivateBundleFromDiskWithBundleRegoVersion(t *testing.T) {
	tests := []struct {
		note              string
		bundleRegoVersion int
		modules           map[string]versionedModule
	}{
		{
			note:              "v0 bundle",
			bundleRegoVersion: 0,
			modules: map[string]versionedModule{
				"policy.rego": {-1, `package config

labels.x := "label value changed"
default_decision := "bar/baz"
default_authorization_decision := "baz/qux"
plugins.test_plugin := v {
	v := {"a": "b"}
}

services.acmecorp.url := v {
	v := "http://localhost:8181"
}

bundles.authz.service := v {
	v := "localhost"
}`},
			},
		},
		{
			note:              "v0 bundle, v1 per-file override",
			bundleRegoVersion: 0,
			modules: map[string]versionedModule{
				"policy1.rego": {-1, `package config

labels.x := "label value changed"
default_decision := "bar/baz"
default_authorization_decision := "baz/qux"
plugins.test_plugin := v {
	v := {"a": "b"}
}`},
				"policy2.rego": {1, `package config

services.acmecorp.url := v if {
	v := "http://localhost:8181"
}

bundles.authz.service := v if {
	v := "localhost"
}`},
			},
		},
		{
			note:              "v1 bundle",
			bundleRegoVersion: 1,
			// no future.keywords import
			modules: map[string]versionedModule{
				"policy.rego": {-1, `package config
labels.x := "label value changed"
default_decision := "bar/baz"
default_authorization_decision := "baz/qux"
plugins.test_plugin := v if {
	v := {"a": "b"}
}
services.acmecorp.url := v if {
	v := "http://localhost:8181"
}
bundles.authz.service := v if {
	v := "localhost"
}`},
			},
		},
		{
			note:              "v1 bundle, v0 per-file override",
			bundleRegoVersion: 1,
			modules: map[string]versionedModule{
				"policy1.rego": {0, `package config

labels.x := "label value changed"
default_decision := "bar/baz"
default_authorization_decision := "baz/qux"
plugins.test_plugin := v {
	v := {"a": "b"}
}`},
				"policy2.rego": {-1, `package config

services.acmecorp.url := v if {
	v := "http://localhost:8181"
}

bundles.authz.service := v if {
	v := "localhost"
}`},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.note, func(t *testing.T) {
			dir := t.TempDir()

			manager, err := plugins.New([]byte(`{
		"labels": {"x": "y"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
		"discovery": {"name": "config", "persist": true},
	}`), "test-id",
				inmem.New())
			if err != nil {
				t.Fatal(err)
			}

			testPlugin := &reconfigureTestPlugin{counts: map[string]int{}}
			testFactory := testFactory{p: testPlugin}

			disco, err := New(manager, Factories(map[string]plugins.Factory{"test_plugin": testFactory}))
			if err != nil {
				t.Fatal(err)
			}

			ctx := context.Background()

			disco.bundlePersistPath = filepath.Join(dir, ".opa")

			ensurePluginState(t, disco, plugins.StateNotReady)

			// persist a bundle to disk and then load it
			initialBundle := makeBundleWithRegoVersion(1, tc.bundleRegoVersion, tc.modules)

			initialBundle.Manifest.Init()

			var buf bytes.Buffer
			if err := bundleApi.NewWriter(&buf).Write(*initialBundle); err != nil {
				t.Fatal("unexpected error:", err)
			}

			err = disco.saveBundleToDisk(&buf)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}

			disco.loadAndActivateBundleFromDisk(ctx)

			ensurePluginState(t, disco, plugins.StateOK)

			// verify the test plugin was registered on the manager
			if plugin := manager.Plugin("test_plugin"); plugin == nil {
				t.Fatalf("expected \"test_plugin\" to be regsitered with the plugin manager")
			}

			// verify the test plugin was started
			count, ok := testPlugin.counts["start"]
			if !ok {
				t.Fatal("expected test plugin to have start counter")
			}

			if count != 1 {
				t.Fatalf("expected test plugin to have a start count of 1 but got %v", count)
			}

			// verify the bundle plugin was registered on the manager
			if plugin := bundlePlugin.Lookup(disco.manager); plugin == nil {
				t.Fatalf("expected bundle plugin to be regsitered with the plugin manager")
			}
		})
	}
}

func TestSaveBundleToDiskNew(t *testing.T) {
	dir := t.TempDir()

	manager, err := plugins.New([]byte(`{
		"labels": {"x": "y"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
		"discovery": {"name": "config", "persist": true},
	}`), "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	testPlugin := &reconfigureTestPlugin{counts: map[string]int{}}
	testFactory := testFactory{p: testPlugin}

	disco, err := New(manager, Factories(map[string]plugins.Factory{"test_plugin": testFactory}))
	if err != nil {
		t.Fatal(err)
	}

	disco.bundlePersistPath = filepath.Join(dir, ".opa")

	initialBundle := makeDataBundle(1, `
		{
			"config": {
				"labels": {"x": "label value changed"},
				"default_decision": "bar/baz",
				"default_authorization_decision": "baz/qux",
				"plugins": {
					"test_plugin": {"a": "b"}
				}
			}
		}
	`)

	initialBundle.Manifest.Init()

	var buf bytes.Buffer
	if err := bundleApi.NewWriter(&buf).Write(*initialBundle); err != nil {
		t.Fatal("unexpected error:", err)
	}

	err = disco.saveBundleToDisk(&buf)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestSaveBundleToDiskNewConfiguredPersistDir(t *testing.T) {
	dir := t.TempDir()

	manager, err := plugins.New([]byte(`{
		"labels": {"x": "y"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
		"discovery": {"name": "config", "persist": true},
	}`), "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	// configure persistence dir instead of using the default. Discover plugin should pick this up
	manager.Config.PersistenceDirectory = &dir

	testPlugin := &reconfigureTestPlugin{counts: map[string]int{}}
	testFactory := testFactory{p: testPlugin}

	disco, err := New(manager, Factories(map[string]plugins.Factory{"test_plugin": testFactory}))
	if err != nil {
		t.Fatal(err)
	}

	err = disco.Start(context.Background())
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	initialBundle := makeDataBundle(1, `
		{
			"config": {
				"labels": {"x": "label value changed"},
				"default_decision": "bar/baz",
				"default_authorization_decision": "baz/qux",
				"plugins": {
					"test_plugin": {"a": "b"}
				}
			}
		}
	`)

	initialBundle.Manifest.Init()

	var buf bytes.Buffer
	if err := bundleApi.NewWriter(&buf).Write(*initialBundle); err != nil {
		t.Fatal("unexpected error:", err)
	}

	err = disco.saveBundleToDisk(&buf)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	expectBundlePath := filepath.Join(dir, "bundles", "config", "bundle.tar.gz")
	_, err = os.Stat(expectBundlePath)
	if err != nil {
		t.Errorf("expected bundle persisted at path %v, %v", expectBundlePath, err)
	}
}

func TestReconfigure(t *testing.T) {

	manager, err := plugins.New([]byte(`{
		"labels": {"x": "y"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
		"discovery": {"name": "config"},
	}`), "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	testPlugin := &reconfigureTestPlugin{counts: map[string]int{}}
	testFactory := testFactory{p: testPlugin}

	disco, err := New(manager, Factories(map[string]plugins.Factory{"test_plugin": testFactory}))
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	initialBundle := makeDataBundle(1, `
		{
			"config": {
				"labels": {"x": "label value changed", "y": "new label"},
				"default_decision": "bar/baz",
				"default_authorization_decision": "baz/qux",
				"plugins": {
					"test_plugin": {"a": "b"}
				}
			}
		}
	`)

	disco.oneShot(ctx, download.Update{Bundle: initialBundle, Size: snapshotBundleSize})

	if disco.status == nil {
		t.Fatal("Expected to find status, found nil")
	} else if disco.status.Type != bundleApi.SnapshotBundleType {
		t.Fatalf("expected snapshot bundle but got %v", disco.status.Type)
	} else if disco.status.Size != snapshotBundleSize {
		t.Fatalf("expected snapshot bundle size %d but got %d", snapshotBundleSize, disco.status.Size)
	}

	// Verify labels are unchanged but allow additions
	exp := map[string]string{"x": "y", "y": "new label", "id": "test-id", "version": version.Version}
	if !maps.Equal(manager.Labels(), exp) {
		t.Errorf("Expected labels to be unchanged (%v) but got %v", exp, manager.Labels())
	}

	// Verify decision ids set
	expDecision := ast.MustParseTerm("data.bar.baz")
	expAuthzDecision := ast.MustParseTerm("data.baz.qux")
	if !manager.Config.DefaultDecisionRef().Equal(expDecision.Value) {
		t.Errorf("Expected default decision to be %v but got %v", expDecision, manager.Config.DefaultDecisionRef())
	}
	if !manager.Config.DefaultAuthorizationDecisionRef().Equal(expAuthzDecision.Value) {
		t.Errorf("Expected default authz decision to be %v but got %v", expAuthzDecision, manager.Config.DefaultAuthorizationDecisionRef())
	}

	// Verify plugins started
	if !maps.Equal(testPlugin.counts, map[string]int{"start": 1}) {
		t.Errorf("Expected exactly one plugin start but got %v", testPlugin)
	}

	// Verify plugins reconfigured
	updatedBundle := makeDataBundle(2, `
		{
			"config": {
				"labels": {"x": "label value changed", "z": "another added label" },
				"default_decision": "bar/baz",
				"default_authorization_decision": "baz/qux",
				"plugins": {
					"test_plugin": {"a": "plugin parameter value changed"}
				}
			}
		}
	`)

	disco.oneShot(ctx, download.Update{Bundle: updatedBundle})

	// Verify label additions are always on top of bootstrap config with multiple discovery documents
	exp = map[string]string{"x": "y", "z": "another added label", "id": "test-id", "version": version.Version}
	if !maps.Equal(manager.Labels(), exp) {
		t.Errorf("Expected labels to be unchanged (%v) but got %v", exp, manager.Labels())
	}

	if disco.status == nil {
		t.Fatal("Expected to find status, found nil")
	} else if disco.status.Type != bundleApi.SnapshotBundleType {
		t.Fatalf("expected snapshot bundle but got %v", disco.status.Type)
	}

	if !maps.Equal(testPlugin.counts, map[string]int{"start": 1, "reconfig": 1}) {
		t.Errorf("Expected one plugin start and one reconfig but got %v", testPlugin)
	}
}

func TestReconfigureV1Compatible(t *testing.T) {
	popts := ast.ParserOptions{RegoVersion: ast.RegoV1}

	manager, err := plugins.New([]byte(`{
		"labels": {"x": "y"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
		"discovery": {"name": "config"},
	}`), "test-id",
		inmem.New(),
		plugins.WithParserOptions(popts))
	if err != nil {
		t.Fatal(err)
	}

	testPlugin := &reconfigureTestPlugin{counts: map[string]int{}}
	testFactory := testFactory{p: testPlugin}

	disco, err := New(manager, Factories(map[string]plugins.Factory{"test_plugin": testFactory}))
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	initialBundle := makeModuleBundle(1, `package config
labels := v if {
	v := {"x": "label value changed", "y": "new label"}
}
default_decision := "bar/baz"
default_authorization_decision := "baz/qux"
plugins.test_plugin := v if {
	v := {"a": "b"}
}`, popts)

	disco.oneShot(ctx, download.Update{Bundle: initialBundle, Size: snapshotBundleSize})

	if disco.status == nil {
		t.Fatal("Expected to find status, found nil")
	} else if disco.status.Type != bundleApi.SnapshotBundleType {
		t.Fatalf("expected snapshot bundle but got %v", disco.status.Type)
	} else if disco.status.Size != snapshotBundleSize {
		t.Fatalf("expected snapshot bundle size %d but got %d", snapshotBundleSize, disco.status.Size)
	}

	// Verify labels are unchanged but allow additions
	exp := map[string]string{"x": "y", "y": "new label", "id": "test-id", "version": version.Version}
	if !maps.Equal(manager.Labels(), exp) {
		t.Errorf("Expected labels to be unchanged (%v) but got %v", exp, manager.Labels())
	}

	// Verify decision ids set
	expDecision := ast.MustParseTerm("data.bar.baz")
	expAuthzDecision := ast.MustParseTerm("data.baz.qux")
	if !manager.Config.DefaultDecisionRef().Equal(expDecision.Value) {
		t.Errorf("Expected default decision to be %v but got %v", expDecision, manager.Config.DefaultDecisionRef())
	}
	if !manager.Config.DefaultAuthorizationDecisionRef().Equal(expAuthzDecision.Value) {
		t.Errorf("Expected default authz decision to be %v but got %v", expAuthzDecision, manager.Config.DefaultAuthorizationDecisionRef())
	}

	// Verify plugins started
	if !maps.Equal(testPlugin.counts, map[string]int{"start": 1}) {
		t.Errorf("Expected exactly one plugin start but got %v", testPlugin)
	}

	// Verify plugins reconfigured
	updatedBundle := makeModuleBundle(2, `package config
labels := v if {
	v := {"x": "label value changed", "z": "another added label"}
}
default_decision := "bar/baz"
default_authorization_decision := "baz/qux"
plugins.test_plugin := v if {
	v := {"a": "plugin parameter value changed"}
}`, popts)

	disco.oneShot(ctx, download.Update{Bundle: updatedBundle})

	// Verify label additions are always on top of bootstrap config with multiple discovery documents
	exp = map[string]string{"x": "y", "z": "another added label", "id": "test-id", "version": version.Version}
	if !maps.Equal(manager.Labels(), exp) {
		t.Errorf("Expected labels to be unchanged (%v) but got %v", exp, manager.Labels())
	}

	if disco.status == nil {
		t.Fatal("Expected to find status, found nil")
	} else if disco.status.Type != bundleApi.SnapshotBundleType {
		t.Fatalf("expected snapshot bundle but got %v", disco.status.Type)
	}

	if !maps.Equal(testPlugin.counts, map[string]int{"start": 1, "reconfig": 1}) {
		t.Errorf("Expected one plugin start and one reconfig but got %v", testPlugin)
	}

	regoV0Bundle := makeModuleBundleWithRegoVersion(2, `package config
labels := v {
	v := {"a": "zero"}
}
default_decision := "bar/baz"
default_authorization_decision := "baz/qux"`, 0)

	disco.oneShot(ctx, download.Update{Bundle: regoV0Bundle})

	if disco.status == nil {
		t.Fatal("Expected to find status, found nil")
	} else if disco.status.Type != bundleApi.SnapshotBundleType {
		t.Fatalf("expected snapshot bundle but got %v", disco.status.Type)
	}

	expLabel := "zero"
	actLabel := manager.Labels()["a"]
	if actLabel != expLabel {
		t.Errorf(`Expected label "a" to be: %v, got: %v`, expLabel, actLabel)
	}

	regoV1Bundle := makeModuleBundleWithRegoVersion(2, `package config
labels := v if {
	v := {"a": "one"}
}
default_decision := "bar/baz"
default_authorization_decision := "baz/qux"`, 1)

	disco.oneShot(ctx, download.Update{Bundle: regoV1Bundle})

	if disco.status == nil {
		t.Fatal("Expected to find status, found nil")
	} else if disco.status.Type != bundleApi.SnapshotBundleType {
		t.Fatalf("expected snapshot bundle but got %v", disco.status.Type)
	}

	expLabel = "one"
	actLabel = manager.Labels()["a"]
	if actLabel != expLabel {
		t.Errorf(`Expected label "a" to be: %v, got: %v`, expLabel, actLabel)
	}
}

func TestReconfigureWithBundleRegoVersion(t *testing.T) {
	popts := ast.ParserOptions{RegoVersion: ast.RegoV1}

	manager, err := plugins.New([]byte(`{
		"labels": {"x": "y"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
		"discovery": {"name": "config"},
	}`), "test-id",
		inmem.New(),
		plugins.WithParserOptions(popts))
	if err != nil {
		t.Fatal(err)
	}

	testPlugin := &reconfigureTestPlugin{counts: map[string]int{}}
	testFactory := testFactory{p: testPlugin}

	disco, err := New(manager, Factories(map[string]plugins.Factory{"test_plugin": testFactory}))
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	initialBundle := makeModuleBundleWithRegoVersion(1, `package config
labels := v if {
	v := {"x": "label value changed", "y": "new label"}
}
default_decision := "bar/baz"
default_authorization_decision := "baz/qux"
plugins.test_plugin := v if {
	v := {"a": "b"}
}`, 1)

	disco.oneShot(ctx, download.Update{Bundle: initialBundle, Size: snapshotBundleSize})

	if disco.status == nil {
		t.Fatal("Expected to find status, found nil")
	} else if disco.status.Type != bundleApi.SnapshotBundleType {
		t.Fatalf("expected snapshot bundle but got %v", disco.status.Type)
	} else if disco.status.Size != snapshotBundleSize {
		t.Fatalf("expected snapshot bundle size %d but got %d", snapshotBundleSize, disco.status.Size)
	}

	// Verify labels are unchanged but allow additions
	exp := map[string]string{"x": "y", "y": "new label", "id": "test-id", "version": version.Version}
	if !maps.Equal(manager.Labels(), exp) {
		t.Errorf("Expected labels to be unchanged (%v) but got %v", exp, manager.Labels())
	}

	// Verify decision ids set
	expDecision := ast.MustParseTerm("data.bar.baz")
	expAuthzDecision := ast.MustParseTerm("data.baz.qux")
	if !manager.Config.DefaultDecisionRef().Equal(expDecision.Value) {
		t.Errorf("Expected default decision to be %v but got %v", expDecision, manager.Config.DefaultDecisionRef())
	}
	if !manager.Config.DefaultAuthorizationDecisionRef().Equal(expAuthzDecision.Value) {
		t.Errorf("Expected default authz decision to be %v but got %v", expAuthzDecision, manager.Config.DefaultAuthorizationDecisionRef())
	}

	// Verify plugins started
	if !maps.Equal(testPlugin.counts, map[string]int{"start": 1}) {
		t.Errorf("Expected exactly one plugin start but got %v", testPlugin)
	}

	// Verify plugins reconfigured
	updatedBundle := makeModuleBundleWithRegoVersion(2, `package config
labels := v if {
	v := {"x": "label value changed", "z": "another added label"}
}
default_decision := "bar/baz"
default_authorization_decision := "baz/qux"
plugins.test_plugin := v if {
	v := {"a": "plugin parameter value changed"}
}`, 1)

	disco.oneShot(ctx, download.Update{Bundle: updatedBundle})

	// Verify label additions are always on top of bootstrap config with multiple discovery documents
	exp = map[string]string{"x": "y", "z": "another added label", "id": "test-id", "version": version.Version}
	if !maps.Equal(manager.Labels(), exp) {
		t.Errorf("Expected labels to be unchanged (%v) but got %v", exp, manager.Labels())
	}

	if disco.status == nil {
		t.Fatal("Expected to find status, found nil")
	} else if disco.status.Type != bundleApi.SnapshotBundleType {
		t.Fatalf("expected snapshot bundle but got %v", disco.status.Type)
	}

	if !maps.Equal(testPlugin.counts, map[string]int{"start": 1, "reconfig": 1}) {
		t.Errorf("Expected one plugin start and one reconfig but got %v", testPlugin)
	}
}

func TestReconfigureWithLocalOverride(t *testing.T) {
	ctx := context.Background()

	bootConfigRaw := []byte(`{
		"labels": {"x": "y"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
		"discovery": {"name": "config"},
        "default_decision": "/http/example/authz/allow",
		"keys": {
			"local_key": {
				"key": "some_private_key",
				"scope": "write"
			}
		},
        "decision_logs": {"console": true},
        "nd_builtin_cache": false,
        "distributed_tracing": {"type": "grpc"},
        "caching": {
			"inter_query_builtin_cache": {"max_size_bytes": 10000000, "forced_eviction_threshold_percentage": 90},
			"inter_query_builtin_value_cache": {
				"named": {
					"io_jwt": {"max_num_entries": 55},
					"graphql": {"max_num_entries": 10}
				}
			}
		}
	}`)

	manager, err := plugins.New(bootConfigRaw, "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	var bootConfig map[string]any
	err = util.Unmarshal(bootConfigRaw, &bootConfig)
	if err != nil {
		t.Fatal(err)
	}

	disco, err := New(manager, BootConfig(bootConfig))
	if err != nil {
		t.Fatal(err)
	}

	// new label added in service config and boot config overrides existing label
	serviceBundle := makeDataBundle(1, `
		{
			"config": {
				"labels": {"x": "new_value", "y": "new label"}
			}
		}
	`)

	disco.oneShot(ctx, download.Update{Bundle: serviceBundle})

	if disco.status == nil {
		t.Fatal("Expected to find status, found nil")
	} else if !strings.Contains(disco.status.Message, "labels.x") {
		t.Fatal("expected key \"labels.x\" to be overridden")
	}

	exp := map[string]string{"x": "y", "y": "new label", "id": "test-id", "version": version.Version}
	if !maps.Equal(manager.Labels(), exp) {
		t.Errorf("Expected labels (%v) but got %v", exp, manager.Labels())
	}

	// `default_authorization_decision` is not specified in the boot config. Hence, it will get a default value.
	// We're specifying it in the service config so, it should take precedence.
	serviceBundle = makeDataBundle(2, `
		{
			"config": {
				"default_authorization_decision": "/http/example/system/allow"
			}
		}
	`)

	disco.oneShot(ctx, download.Update{Bundle: serviceBundle})

	expAuthzRule := "/http/example/system/allow"
	if *manager.Config.DefaultAuthorizationDecision != expAuthzRule {
		t.Errorf("Expected default authorization decision %v but got %v", expAuthzRule, *manager.Config.DefaultAuthorizationDecision)
	}

	// `default_decision` is specified in both boot and service config. The former should take precedence.
	serviceBundle = makeDataBundle(3, `
		{
			"config": {
				"default_decision": "/http/example/authz/allow/something"
			}
		}
	`)

	disco.oneShot(ctx, download.Update{Bundle: serviceBundle})

	if disco.status == nil {
		t.Fatal("Expected to find status, found nil")
	} else if !strings.Contains(disco.status.Message, "default_decision") {
		t.Fatal("expected key \"default_decision\" to be overridden")
	}

	expAuthzRule = "/http/example/authz/allow"
	if *manager.Config.DefaultDecision != expAuthzRule {
		t.Fatalf("Expected default decision %v but got %v", expAuthzRule, *manager.Config.DefaultDecision)
	}

	// `nd_builtin_cache` is specified in both boot and service config. The former should take precedence.
	serviceBundle = makeDataBundle(4, `
		{
			"config": {
				"nd_builtin_cache": true
			}
		}
	`)

	disco.oneShot(ctx, download.Update{Bundle: serviceBundle})

	if disco.status == nil {
		t.Fatal("Expected to find status, found nil")
	} else if !strings.Contains(disco.status.Message, "nd_builtin_cache") {
		t.Fatal("expected key \"nd_builtin_cache\" to be overridden")
	}

	if manager.Config.NDBuiltinCache {
		t.Fatal("Expected nd_builtin_cache value to be false")
	}

	// `persistence_directory` not specified in boot config. The service config value should be used.
	serviceBundle = makeDataBundle(5, `
		{
			"config": {
				"persistence_directory": "test"
			}
		}
	`)

	disco.oneShot(ctx, download.Update{Bundle: serviceBundle})

	if manager.Config.PersistenceDirectory == nil || *manager.Config.PersistenceDirectory != "test" {
		t.Fatal("Unexpected update to persistence directory")
	}

	// nested: overriding a value in an existing config
	serviceBundle = makeDataBundle(6, `
		{
			"config": {
				"decision_logs": {"console": false, "reporting": {"max_delay_seconds": 15, "min_delay_seconds": 10}}
			}
		}
	`)

	disco.oneShot(ctx, download.Update{Bundle: serviceBundle})

	if disco.status == nil {
		t.Fatal("Expected to find status, found nil")
	} else if !strings.Contains(disco.status.Message, "decision_logs.console") {
		t.Fatal("expected key \"decision_logs.console\" to be overridden")
	}

	dlPlugin := logs.Lookup(disco.manager)
	config := dlPlugin.Config()

	actualMax := time.Duration(*config.Reporting.MaxDelaySeconds) / time.Nanosecond
	expectedMax := time.Duration(15) * time.Second

	if actualMax != expectedMax {
		t.Fatalf("Expected maximum polling interval: %v but got %v", expectedMax, actualMax)
	}

	actualMin := time.Duration(*config.Reporting.MinDelaySeconds) / time.Nanosecond
	expectedMin := time.Duration(10) * time.Second

	if actualMin != expectedMin {
		t.Fatalf("Expected maximum polling interval: %v but got %v", expectedMin, actualMin)
	}

	if !config.ConsoleLogs {
		t.Fatal("Expected console decision logging to be enabled")
	}

	// nested: adding a value in an existing config
	// only `stale_entry_eviction_period_seconds` should be used from the service config as the boot config defines
	// the other fields
	serviceBundle = makeDataBundle(7, `
		{
			"config": {
				"caching": {
					"inter_query_builtin_cache": {"max_size_bytes": 200, "stale_entry_eviction_period_seconds": 10, "forced_eviction_threshold_percentage": 200},
					"inter_query_builtin_value_cache": {
						"named": {
							"io_jwt": {"max_num_entries": 10},
							"graphql": {"max_num_entries": 11}
						}
					}
				}
			}
		}
	`)

	disco.oneShot(ctx, download.Update{Bundle: serviceBundle})

	if disco.status == nil {
		t.Fatal("Expected to find status, found nil")
	}

	expectedOverriddenKeys := []string{
		"caching.inter_query_builtin_cache.max_size_bytes",
		"caching.inter_query_builtin_cache.forced_eviction_threshold_percentage",
		"caching.inter_query_builtin_value_cache.named.io_jwt.max_num_entries",
		"caching.inter_query_builtin_value_cache.named.graphql.max_num_entries",
	}
	for _, k := range expectedOverriddenKeys {
		if !strings.Contains(disco.status.Message, k) {
			t.Fatalf("expected key \"%v\" to be overridden", k)
		}
	}

	cacheConf, err := cache.ParseCachingConfig(manager.Config.Caching)
	if err != nil {
		t.Fatal(err)
	}

	maxSize := new(int64)
	*maxSize = 10000000
	period := new(int64)
	*period = 10
	threshold := new(int64)
	*threshold = 90
	maxNumEntriesInterQueryValueCache := new(int)
	*maxNumEntriesInterQueryValueCache = 0
	maxNumEntriesJWTValueCache := new(int)
	*maxNumEntriesJWTValueCache = 55
	maxNumEntriesGraphQLValueCache := new(int)
	*maxNumEntriesGraphQLValueCache = 10

	expectedCacheConf := &cache.Config{
		InterQueryBuiltinCache: cache.InterQueryBuiltinCacheConfig{
			MaxSizeBytes:                      maxSize,
			StaleEntryEvictionPeriodSeconds:   period,
			ForcedEvictionThresholdPercentage: threshold,
		},
		InterQueryBuiltinValueCache: cache.InterQueryBuiltinValueCacheConfig{
			MaxNumEntries: maxNumEntriesInterQueryValueCache,
			NamedCacheConfigs: map[string]*cache.NamedValueCacheConfig{
				"io_jwt": {
					MaxNumEntries: maxNumEntriesJWTValueCache,
				},
				"graphql": {
					MaxNumEntries: maxNumEntriesGraphQLValueCache,
				},
			},
		},
	}

	if !reflect.DeepEqual(cacheConf, expectedCacheConf) {
		t.Fatalf("want %v got %v", expectedCacheConf, cacheConf)
	}

	// no corresponding service config entry
	serviceBundle = makeDataBundle(8, `
		{
			"config": {}
		}
	`)

	disco.oneShot(ctx, download.Update{Bundle: serviceBundle})

	var dtConfig map[string]any
	err = util.Unmarshal(manager.Config.DistributedTracing, &dtConfig)
	if err != nil {
		t.Fatal(err)
	}

	ty, ok := dtConfig["type"]
	if !ok {
		t.Fatal("Expected config for distributed tracing")
	}

	if ty != "grpc" {
		t.Fatalf("Expected distributed tracing \"grpc\" but got %v", ty)
	}
}

func TestMergeValuesAndListOverrides(t *testing.T) {
	tests := []struct {
		name     string
		dest     map[string]any
		src      map[string]any
		expected map[string]any
		override []string
	}{
		{
			name: "Simple merge",
			dest: map[string]any{
				"a": 1,
				"b": 2,
			},
			src: map[string]any{
				"c": 3,
				"d": 4,
			},
			expected: map[string]any{
				"a": 1,
				"b": 2,
				"c": 3,
				"d": 4,
			},
			override: []string{},
		},
		{
			name: "Nested merge",
			dest: map[string]any{
				"a": 1,
				"b": map[string]any{
					"ba": 10,
				},
			},
			src: map[string]any{
				"b": map[string]any{
					"bb": 20,
				},
				"c": map[string]any{
					"ca": 30,
				},
			},
			expected: map[string]any{
				"a": 1,
				"b": map[string]any{
					"ba": 10,
					"bb": 20,
				},
				"c": map[string]any{
					"ca": 30,
				},
			},
			override: []string{},
		},
		{
			name: "Simple Non-map override -1",
			dest: map[string]any{
				"a": []any{"bar"},
				"b": 2,
			},
			src: map[string]any{
				"a": 3,
			},
			expected: map[string]any{
				"a": 3,
				"b": 2,
			},
			override: []string{"a"},
		},
		{
			name: "Simple Non-map override -2",
			dest: map[string]any{
				"a": 3,
				"b": 2,
			},
			src: map[string]any{
				"a": []any{"bar"},
			},
			expected: map[string]any{
				"a": []any{"bar"},
				"b": 2,
			},
			override: []string{"a"},
		},
		{
			name: "Non-map override -1",
			dest: map[string]any{
				"a": []any{"bar"},
				"b": 2,
			},
			src: map[string]any{
				"a": []string{"foo"},
			},
			expected: map[string]any{
				"a": []string{"foo"},
				"b": 2,
			},
			override: []string{"a"},
		},
		{
			name: "Non-map override -2",
			dest: map[string]any{
				"a": map[string]any{
					"aa": 10,
					"ab": 20,
				},
				"b": 2,
			},
			src: map[string]any{
				"a": []any{"foo"},
			},
			expected: map[string]any{
				"a": []any{"foo"},
				"b": 2,
			},
			override: []string{"a"},
		},
		{
			name: "Simple overridden keys",
			dest: map[string]any{
				"a": 1,
				"b": 2,
			},
			src: map[string]any{
				"b": 20,
				"c": 3,
			},
			expected: map[string]any{
				"a": 1,
				"b": 20,
				"c": 3,
			},
			override: []string{"b"},
		},
		{
			name: "Nested overridden keys",
			dest: map[string]any{
				"a": 1,
				"b": map[string]any{
					"ba": 10,
					"bb": 20,
				},
			},
			src: map[string]any{
				"b": map[string]any{
					"bb": 200,
					"bc": 300,
				},
				"c": map[string]any{
					"ca": 30,
				},
			},
			expected: map[string]any{
				"a": 1,
				"b": map[string]any{
					"ba": 10,
					"bb": 200,
					"bc": 300,
				},
				"c": map[string]any{
					"ca": 30,
				},
			},
			override: []string{"b.bb"},
		},
		{
			name: "Multiple Nested overridden keys",
			dest: map[string]any{
				"a": 1,
				"b": map[string]any{
					"ba": 10,
					"bb": 20,
				},
				"c": map[string]any{
					"ca": 10,
					"cb": 20,
					"cc": 30,
				},
			},
			src: map[string]any{
				"b": map[string]any{
					"bb": 200,
					"bc": 300,
				},
				"c": map[string]any{
					"ca": 300,
					"cd": 400,
				},
			},
			expected: map[string]any{
				"a": 1,
				"b": map[string]any{
					"ba": 10,
					"bb": 200,
					"bc": 300,
				},
				"c": map[string]any{
					"ca": 300,
					"cb": 20,
					"cc": 30,
					"cd": 400,
				},
			},
			override: []string{"b.bb", "c.ca"},
		},
		{
			name: "Nested overridden keys - 2",
			dest: map[string]any{
				"a": 1,
				"b": map[string]any{
					"ba": map[string]any{"bba": "1"},
				},
				"c": 2,
			},
			src: map[string]any{
				"b": map[string]any{
					"ba": map[string]any{"bba": "2"},
				},
				"c": map[string]any{
					"ca": 30,
				},
			},
			expected: map[string]any{
				"a": 1,
				"b": map[string]any{
					"ba": map[string]any{"bba": "2"},
				},
				"c": map[string]any{
					"ca": 30,
				},
			},
			override: []string{"b.ba.bba", "c"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, overriddenKeys := mergeValuesAndListOverrides(tc.dest, tc.src, "")
			if !reflect.DeepEqual(result, tc.expected) {
				t.Errorf("Expected result %v but got %v", tc.expected, result)
			}

			if len(overriddenKeys) != len(tc.override) {
				t.Fatal("Mismatch between expected and actual overridden keys")
			}

			for _, k1 := range tc.override {
				found := false
				for _, k2 := range overriddenKeys {
					if k1 == k2 {
						found = true
					}
				}

				if !found {
					t.Errorf("Expected overridden keys %v but got %v", tc.override, overriddenKeys)
				}
			}
		})
	}
}

func TestReconfigureWithUpdates(t *testing.T) {

	ctx := context.Background()

	bootConfigRaw := []byte(`{
		"labels": {"x": "y"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
		"discovery": {"name": "config"},
		"keys": {
			"global_key": {
				"key": "secret",
				"algorithm": "HS256",
				"scope": "read"
			},
			"local_key": {
				"key": "some_private_key",
				"scope": "write"
			}
		},
		"persistence_directory": "test"
	}`)

	manager, err := plugins.New(bootConfigRaw, "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	var bootConfig map[string]any
	err = util.Unmarshal(bootConfigRaw, &bootConfig)
	if err != nil {
		t.Fatal(err)
	}

	disco, err := New(manager, BootConfig(bootConfig))
	if err != nil {
		t.Fatal(err)
	}
	originalConfig := disco.config

	initialBundle := makeDataBundle(1, `
		{
			"config": {
				"bundle": {"name": "test1"},
				"status": {},
				"decision_logs": {}
			}
		}
	`)

	err = disco.reconfigure(ctx, download.Update{Bundle: initialBundle})
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	// update the discovery configuration and check
	// the boot configuration is not overwritten
	updatedBundle := makeDataBundle(2, `
		{
			"config": {
				"discovery": {
					"name": "config",
					"decision": "/foo/bar"
				}
			}
		}
	`)

	err = disco.reconfigure(ctx, download.Update{Bundle: updatedBundle})
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	if !reflect.DeepEqual(originalConfig, disco.config) {
		t.Fatal("Discovery configuration updated")
	}

	// no update to the discovery configuration and check no error generated
	updatedBundle = makeDataBundle(3, `
		{
			"config": {
				"discovery": {
					"name": "config"
				}
			}
		}
	`)

	err = disco.reconfigure(ctx, download.Update{Bundle: updatedBundle})
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	if !reflect.DeepEqual(originalConfig, disco.config) {
		t.Fatal("Discovery configuration updated")
	}

	// update the discovery service and check that error generated
	updatedBundle = makeDataBundle(4, `
		{
			"config": {
				"services": {
					"localhost": {
						"url": "http://localhost:9999",
						"credentials": {"bearer": {"token": "blah"}}
					}
				}
			}
		}
	`)

	err = disco.reconfigure(ctx, download.Update{Bundle: updatedBundle})
	if err == nil {
		t.Fatal("Expected error but got nil")
	}

	expectedErrMsg := "updates to the discovery service are not allowed"
	if err.Error() != expectedErrMsg {
		t.Fatalf("Expected error message: %v but got: %v", expectedErrMsg, err.Error())
	}

	// no update to the discovery service and check no error generated
	updatedBundle = makeDataBundle(5, `
		{
			"config": {
				"services": {
					"localhost": {
						"url": "http://localhost:9999"
					}
				}
			}
		}
	`)

	err = disco.reconfigure(ctx, download.Update{Bundle: updatedBundle})
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	// add a new service and a new bundle
	updatedBundle = makeDataBundle(6, `
		{
			"config": {
				"services": {
					"acmecorp": {
						"url": "http://localhost:8181"
					}
				},
				"bundles": {
					"authz": {
						"service": "acmecorp"
					}
				}
			}
		}
	`)

	err = disco.reconfigure(ctx, download.Update{Bundle: updatedBundle})
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	if len(disco.manager.Services()) != 2 {
		t.Fatalf("Expected two services but got %v\n", len(disco.manager.Services()))
	}

	bPlugin := bundlePlugin.Lookup(disco.manager)
	config := bPlugin.Config()
	expected := "acmecorp"
	if config.Bundles["authz"].Service != expected {
		t.Fatalf("Expected service %v for bundle authz but got %v", expected, config.Bundles["authz"].Service)
	}

	// update existing bundle's config and add a new bundle
	updatedBundle = makeDataBundle(7, `
		{
			"config": {
				"bundles": {
					"authz": {
						"service": "localhost",
						"resource": "foo/bar"
					},
					"main": {
						"resource": "baz/bar"
					}
				}
			}
		}
	`)

	err = disco.reconfigure(ctx, download.Update{Bundle: updatedBundle})
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	bPlugin = bundlePlugin.Lookup(disco.manager)
	config = bPlugin.Config()
	expectedSvc := "localhost"
	if config.Bundles["authz"].Service != expectedSvc {
		t.Fatalf("Expected service %v for bundle authz but got %v", expectedSvc, config.Bundles["authz"].Service)
	}

	expectedRes := "foo/bar"
	if config.Bundles["authz"].Resource != expectedRes {
		t.Fatalf("Expected resource %v for bundle authz but got %v", expectedRes, config.Bundles["authz"].Resource)
	}

	expectedSvcs := map[string]bool{"localhost": true, "acmecorp": true}
	if _, ok := expectedSvcs[config.Bundles["main"].Service]; !ok {
		t.Fatalf("Expected service for bundle main to be one of [%v, %v] but got %v", "localhost", "acmecorp", config.Bundles["main"].Service)
	}

	// update existing (non-discovery)service's config
	updatedBundle = makeDataBundle(8, `
		{
			"config": {
				"services": {
					"acmecorp": {
						"url": "http://localhost:8181",
						"credentials": {"bearer": {"token": "blah"}}
						}
				},
				"bundles": {
					"authz": {
						"service": "localhost",
						"resource": "foo/bar"
					}
				}
			}
		}
	`)

	err = disco.reconfigure(ctx, download.Update{Bundle: updatedBundle})
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	// add a new key
	updatedBundle = makeDataBundle(9, `
		{
			"config": {
				"keys": {
					"new_global_key": {
						"key": "secret",
						"algorithm": "HS256",
						"scope": "read"
					}
				}
			}
		}
	`)

	err = disco.reconfigure(ctx, download.Update{Bundle: updatedBundle})
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	// update a key in the boot config
	updatedBundle = makeDataBundle(10, `
		{
			"config": {
				"keys": {
					"global_key": {
						"key": "new_secret",
						"algorithm": "HS256",
						"scope": "read"
					}
				}
			}
		}
	`)

	err = disco.reconfigure(ctx, download.Update{Bundle: updatedBundle})
	if err == nil {
		t.Fatal("Expected error but got nil")
	}

	errMsg := "updates to keys specified in the boot configuration are not allowed"
	if err.Error() != errMsg {
		t.Fatalf("Expected error message: %v but got: %v", errMsg, err.Error())
	}

	// no config change for a key in the boot config
	updatedBundle = makeDataBundle(11, `
		{
			"config": {
				"keys": {
					"global_key": {
						"key": "secret",
						"algorithm": "HS256",
						"scope": "read"
					}
				}
			}
		}
	`)

	err = disco.reconfigure(ctx, download.Update{Bundle: updatedBundle})
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	// update a key not in the boot config
	updatedBundle = makeDataBundle(12, `
		{
			"config": {
				"keys": {
					"new_global_key": {
						"key": "secret",
						"algorithm": "HS256",
						"scope": "write"
					}
				}
			}
		}
	`)

	err = disco.reconfigure(ctx, download.Update{Bundle: updatedBundle})
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	// check that omitting persistence_directory or discovery doesn't remove boot config
	updatedBundle = makeDataBundle(13, `
		{
			"config": {}
		}
	`)

	err = disco.reconfigure(ctx, download.Update{Bundle: updatedBundle})
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	if manager.Config.PersistenceDirectory == nil {
		t.Fatal("Erased persistence directory configuration")
	}
	if manager.Config.Discovery == nil {
		t.Fatal("Erased discovery plugin configuration")
	}

	// update persistence directory in the service config and check that its boot config value is not overridden
	updatedBundle = makeDataBundle(14, `
		{
			"config": {
				"persistence_directory": "my_bundles"
			}
		}
	`)

	err = disco.reconfigure(ctx, download.Update{Bundle: updatedBundle})
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	if manager.Config.PersistenceDirectory == nil || *manager.Config.PersistenceDirectory == "my_bundles" {
		t.Fatal("Unexpected update to persistence directory")
	}
}

func TestProcessBundleWithSigning(t *testing.T) {

	ctx := context.Background()

	manager, err := plugins.New([]byte(`{
		"labels": {"x": "y"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
		"discovery": {"name": "config", "signing": {"keyid": "my_global_key"}},
		"keys": {"my_global_key": {"algorithm": "HS256", "key": "secret"}},
	}`), "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	disco, err := New(manager)
	if err != nil {
		t.Fatal(err)
	}

	initialBundle := makeDataBundle(1, `
		{
			"config": {
				"bundle": {"name": "test1"},
				"status": {},
				"decision_logs": {},
				"keys": {"my_local_key": {"algorithm": "HS256", "key": "new_secret"}}
			}
		}
	`)

	_, err = disco.processBundle(ctx, initialBundle)
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}
}

func TestProcessBundleWithNoSigningConfig(t *testing.T) {
	ctx := context.Background()

	manager, err := plugins.New([]byte(`{
		"labels": {"x": "y"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
		"discovery": {"name": "config"}
	}`), "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	disco, err := New(manager)
	if err != nil {
		t.Fatal(err)
	}

	initialBundle := makeDataBundle(1, `
		{
			"config": {
				"bundles": {"test1": {"service": "localhost"}},
				"keys": {"my_local_key": {"algorithm": "HS256", "key": "new_secret"}}
			}
		}
	`)

	_, err = disco.processBundle(ctx, initialBundle)
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}
}

type testServer struct {
	t       *testing.T
	server  *httptest.Server
	updates chan status.UpdateRequestV1
}

func (ts *testServer) Start() {
	ts.updates = make(chan status.UpdateRequestV1, 100)
	ts.server = httptest.NewServer(http.HandlerFunc(ts.handle))
}

func (ts *testServer) Stop() {
	ts.server.Close()
}

func (ts *testServer) handle(w http.ResponseWriter, r *http.Request) {

	var update status.UpdateRequestV1

	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		ts.t.Fatal(err)
	}

	ts.updates <- update

	w.WriteHeader(200)
}

func TestStatusUpdates(t *testing.T) {

	ts := testServer{t: t}
	ts.Start()
	defer ts.Stop()

	manager, err := plugins.New(fmt.Appendf(nil, `{
			"labels": {"x": "y"},
			"services": {
				"localhost": {
					"url": %q
				}
			},
			"discovery": {"name": "config"},
		}`, ts.server.URL), "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	disco, err := New(manager)
	if err != nil {
		t.Fatal(err)
	}

	updates := make(chan status.UpdateRequestV1, 100)

	ctx := context.Background()

	// Enable status plugin which sends initial update.
	disco.oneShot(ctx, download.Update{ETag: "etag-1", Bundle: makeDataBundle(1, `{
		"config": {
			"status": {}
		}
	}`)})

	// status plugin updates and bundle discovery status update,
	updates <- <-ts.updates
	updates <- <-ts.updates
	updates <- <-ts.updates

	// Downloader error.
	disco.oneShot(ctx, download.Update{Error: errors.New("unknown error")})

	updates <- <-ts.updates

	// Clear error.
	disco.oneShot(ctx, download.Update{ETag: "etag-2", Bundle: makeDataBundle(2, `{
		"config": {
			"status": {}
		}
	}`)})

	updates <- <-ts.updates

	// Configuration error.
	disco.oneShot(ctx, download.Update{ETag: "etag-3", Bundle: makeDataBundle(3, `{
		"config": {
			"status": {"service": "missing service"}
		}
	}`)})

	updates <- <-ts.updates

	// Clear error (last successful reconfigure).
	disco.oneShot(ctx, download.Update{ETag: "etag-2"})

	updates <- <-ts.updates

	// Check that all updates were received and active revisions are expected.
	expectedDiscoveryUpdates := []struct {
		Code     string
		Revision string
	}{
		{
			Code:     "",
			Revision: "test-revision-1",
		},
		{
			Code:     "bundle_error",
			Revision: "test-revision-1",
		},
		{
			Code:     "",
			Revision: "test-revision-2",
		},
		{
			Code:     "bundle_error",
			Revision: "test-revision-2",
		},
		{
			Code:     "",
			Revision: "test-revision-2",
		},
	}

	// nextExpectedDiscoveryUpdate, we look for each
	nextExpectedDiscoveryUpdate := expectedDiscoveryUpdates[0]
	expectedDiscoveryUpdates = expectedDiscoveryUpdates[1:]

	timeout, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	for {
		select {
		case update := <-updates:
			if update.Discovery != nil {
				matches := false
				if update.Discovery.Code == nextExpectedDiscoveryUpdate.Code &&
					update.Discovery.ActiveRevision == nextExpectedDiscoveryUpdate.Revision {
					matches = true
				}

				if matches {
					if len(expectedDiscoveryUpdates) == 0 {
						return
					}
					nextExpectedDiscoveryUpdate = expectedDiscoveryUpdates[0]
					expectedDiscoveryUpdates = expectedDiscoveryUpdates[1:]
				}
			}
		case <-timeout.Done():
			cancel()
			t.Fatalf("Waiting for following statuses timed out: %v", expectedDiscoveryUpdates)
		}
	}
}

func TestStatusUpdatesFromPersistedBundlesDontDelayBoot(t *testing.T) {
	dir := t.TempDir()

	// write the disco bundle to disk
	discoBundle := bundleApi.Bundle{
		Data: map[string]any{
			"discovery": map[string]any{
				"bundles": map[string]any{
					"main": map[string]any{
						"persist":  true,
						"resource": "/bundle",
						"service":  "localhost",
					},
				},
				"status": map[string]any{
					"service": "localhost",
				},
			},
		},
	}

	discoBundleDir := filepath.Join(dir, "bundles", "config")
	if err := os.MkdirAll(discoBundleDir, 0755); err != nil {
		t.Fatal(err)
	}

	discoBundleFile, err := os.Create(filepath.Join(discoBundleDir, "bundle.tar.gz"))
	if err != nil {
		t.Fatal(err)

	}
	defer discoBundleFile.Close()

	err = bundleApi.NewWriter(discoBundleFile).Write(discoBundle)
	if err != nil {
		t.Fatal(err)
	}

	// write an example data bundle ('main') to disk
	mainBundle := bundleApi.Bundle{
		Data: map[string]any{
			"foo": "bar",
		},
	}

	mainBundleDir := filepath.Join(dir, "bundles", "main")
	if err := os.MkdirAll(mainBundleDir, 0755); err != nil {
		t.Fatal(err)
	}

	mainBundleFile, err := os.Create(filepath.Join(mainBundleDir, "bundle.tar.gz"))
	if err != nil {
		t.Fatal(err)

	}
	defer mainBundleFile.Close()

	err = bundleApi.NewWriter(mainBundleFile).Write(mainBundle)
	if err != nil {
		t.Fatal(err)
	}

	// Create a timing out listener for the referenced localhost service
	// :0 will cause net to find an available port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	manager, err := plugins.New(fmt.Appendf(nil, `{
            "persistence_directory": %q,
			"services": {
				"localhost": {
					"url": "http://%s"
				}
			},
			"discovery": {"name": "config", "persist": true, "decision": "discovery"},
		}`, dir, listener.Addr().String()), "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	// allow 2s of time to start before failing
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// start Discovery instance, wait for it to complete Start()
	booted := make(chan bool)
	go func() {
		disco, err := New(manager)
		if err != nil {
			t.Log(err)
			return
		}
		err = disco.Start(ctx)
		if err != nil {
			t.Log(err)
			return
		}
		booted <- true
	}()

	select {
	case <-booted:
		for k, pi := range manager.PluginStatus() {
			if pi.State != plugins.StateOK {
				t.Errorf("Expected %s plugin to be in OK state but got %v", k, pi.State)
			}
		}
	case <-ctx.Done():
		t.Errorf("Timed out waiting for disco to start")
	}
}

func TestStatusUpdatesTimestamp(t *testing.T) {

	ts := testServer{t: t}
	ts.Start()
	defer ts.Stop()

	manager, err := plugins.New(fmt.Appendf(nil, `{
			"labels": {"x": "y"},
			"services": {
				"localhost": {
					"url": %q
				}
			},
			"discovery": {"name": "config"},
		}`, ts.server.URL), "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	disco, err := New(manager)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// simulate HTTP 200 response from downloader
	disco.oneShot(ctx, download.Update{ETag: "etag-1", Bundle: makeDataBundle(1, `{
		"config": {
			"status": {}
		}
	}`)})

	if disco.status.LastSuccessfulDownload != disco.status.LastSuccessfulRequest || disco.status.LastSuccessfulDownload != disco.status.LastRequest {
		t.Fatal("expected last successful request to be same as download and request")
	}

	if disco.status.LastSuccessfulActivation.IsZero() {
		t.Fatal("expected last successful activation to be non-zero")
	}

	time.Sleep(time.Millisecond)

	// simulate HTTP 304 response from downloader
	disco.oneShot(ctx, download.Update{ETag: "etag-1", Bundle: nil})
	if disco.status.LastSuccessfulDownload == disco.status.LastSuccessfulRequest || disco.status.LastSuccessfulDownload == disco.status.LastRequest {
		t.Fatal("expected last successful download to differ from request and last request")
	}

	// simulate HTTP 200 response from downloader
	disco.oneShot(ctx, download.Update{ETag: "etag-2", Bundle: makeDataBundle(2, `{
		"config": {
			"status": {}
		}
	}`)})

	if disco.status.LastSuccessfulDownload != disco.status.LastSuccessfulRequest || disco.status.LastSuccessfulDownload != disco.status.LastRequest {
		t.Fatal("expected last successful request to be same as download and request")
	}

	if disco.status.LastSuccessfulActivation.IsZero() {
		t.Fatal("expected last successful activation to be non-zero")
	}

	// simulate error response from downloader
	disco.oneShot(ctx, download.Update{Error: errors.New("unknown error")})

	if disco.status.LastSuccessfulDownload != disco.status.LastSuccessfulRequest || disco.status.LastSuccessfulDownload == disco.status.LastRequest {
		t.Fatal("expected last successful request to be same as download but different from request")
	}
}

func TestStatusMetricsForLogDrops(t *testing.T) {

	ctx := context.Background()

	testLogger := test.New()

	manager, err := plugins.New([]byte(`{
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
		"discovery": {"name": "config"},
	}`), "test-id", inmem.New(), plugins.ConsoleLogger(testLogger))
	if err != nil {
		t.Fatal(err)
	}

	initialBundle := makeDataBundle(1, `
		{
			"config": {
				"status": {"console": true},
				"decision_logs": {
					"service": "localhost",
					"reporting": {
						"max_decisions_per_second": 1
					}
				}
			}
		}
	`)

	disco, err := New(manager, Metrics(metrics.New()))
	if err != nil {
		t.Fatal(err)
	}

	ps, err := disco.processBundle(ctx, initialBundle)
	if err != nil {
		t.Fatal(err)
	}

	// start the decision log and status plugins
	for _, p := range ps.Start {
		if err := p.Start(ctx); err != nil {
			t.Fatal(err)
		}
	}

	plugin := logs.Lookup(manager)
	if plugin == nil {
		t.Fatal("Expected decision log plugin registered on manager")
	}

	var input any = map[string]any{"method": "GET"}
	var result any = false

	event1 := &server.Info{
		DecisionID: "abc",
		Path:       "foo/bar",
		Input:      &input,
		Results:    &result,
		RemoteAddr: "test-1",
	}

	event2 := &server.Info{
		DecisionID: "def",
		Path:       "foo/baz",
		Input:      &input,
		Results:    &result,
		RemoteAddr: "test-2",
	}

	event3 := &server.Info{
		DecisionID: "ghi",
		Path:       "foo/aux",
		Input:      &input,
		Results:    &result,
		RemoteAddr: "test-3",
	}

	_ = plugin.Log(ctx, event1) // event 1 should be written into the decision log encoder
	_ = plugin.Log(ctx, event2) // event 2 should not be written into the decision log encoder as rate limit exceeded
	_ = plugin.Log(ctx, event3) // event 3 should not be written into the decision log encoder as rate limit exceeded

	// trigger a status update
	disco.oneShot(ctx, download.Update{ETag: "etag-1", Bundle: makeDataBundle(1, `{
		"config": {
			"bundles": {"test-bundle": {"service": "localhost"}}
		}
	}`)})

	status.Lookup(manager).Stop(ctx)

	entries := testLogger.Entries()
	if len(entries) == 0 {
		t.Fatal("Expected log entries but got none")
	}

	// Pick the last entry as it should have the drop count
	e := entries[len(entries)-1]

	if _, ok := e.Fields["metrics"]; !ok {
		t.Fatal("Expected metrics")
	}

	builtInMet := e.Fields["metrics"].(map[string]any)["<built-in>"]
	dropCount := builtInMet.(map[string]any)["counter_decision_logs_dropped_rate_limit_exceeded"]

	actual, err := dropCount.(json.Number).Int64()
	if err != nil {
		t.Fatal(err)
	}

	// Along with event 2 and event 3, event 1 could also get dropped. This happens when the decision log plugin
	// tries to requeue event 1 after a failed upload attempt to a non-existent remote endpoint
	if actual < 2 {
		t.Fatal("Expected at least 2 events to be dropped")
	}
}

func makeDataBundle(n int, s string) *bundleApi.Bundle {
	return &bundleApi.Bundle{
		Manifest: bundleApi.Manifest{Revision: fmt.Sprintf("test-revision-%v", n)},
		Data:     util.MustUnmarshalJSON([]byte(s)).(map[string]any),
	}
}

func makeModuleBundle(n int, s string, popts ast.ParserOptions) *bundleApi.Bundle {
	return &bundleApi.Bundle{
		Manifest: bundleApi.Manifest{Revision: fmt.Sprintf("test-revision-%v", n)},
		Modules: []bundleApi.ModuleFile{
			{
				URL:    `policy.rego`,
				Path:   `/policy.rego`,
				Raw:    []byte(s),
				Parsed: ast.MustParseModuleWithOpts(s, popts),
			},
		},
		Data: map[string]any{},
	}
}

func makeModuleBundleWithRegoVersion(revision int, bundle string, regoVersion int) *bundleApi.Bundle {
	popts := ast.ParserOptions{}
	if regoVersion == 0 {
		popts.RegoVersion = ast.RegoV0
	} else {
		popts.RegoVersion = ast.RegoV1
	}
	return &bundleApi.Bundle{
		Manifest: bundleApi.Manifest{
			Revision:    fmt.Sprintf("test-revision-%v", revision),
			RegoVersion: &regoVersion,
		},
		Modules: []bundleApi.ModuleFile{
			{
				URL:    `policy.rego`,
				Path:   `/policy.rego`,
				Raw:    []byte(bundle),
				Parsed: ast.MustParseModuleWithOpts(bundle, popts),
			},
		},
		Data: map[string]any{},
	}
}

type versionedModule struct {
	version int
	module  string
}

func makeBundleWithRegoVersion(revision int, bundleRegoVersion int, modules map[string]versionedModule) *bundleApi.Bundle {
	b := bundleApi.Bundle{
		Manifest: bundleApi.Manifest{
			Revision:         fmt.Sprintf("test-revision-%v", revision),
			RegoVersion:      &bundleRegoVersion,
			FileRegoVersions: map[string]int{},
		},
		Data: map[string]any{},
	}

	for k, v := range modules {
		p := path.Join("/", k)
		popts := ast.ParserOptions{}
		if v.version >= 0 {
			b.Manifest.FileRegoVersions[p] = v.version
			popts.RegoVersion = ast.RegoVersionFromInt(v.version)
		} else {
			popts.RegoVersion = ast.RegoVersionFromInt(bundleRegoVersion)
		}
		b.Modules = append(b.Modules, bundleApi.ModuleFile{
			URL:    k,
			Path:   p,
			Raw:    []byte(v.module),
			Parsed: ast.MustParseModuleWithOpts(v.module, popts),
		})
	}

	return &b
}

func getTestManager(t *testing.T, conf string) *plugins.Manager {
	t.Helper()
	store := inmem.New()
	manager, err := plugins.New([]byte(conf), "test-instance-id", store)
	if err != nil {
		t.Fatalf("failed to create plugin manager: %s", err)
	}
	return manager
}

func TestGetPluginSetWithMixedConfig(t *testing.T) {
	conf := `
services:
  s1:
    url: http://test1.com
  s2:
    url: http://test2.com

bundles:
  bundle-new:
    service: s1

bundle:
  name: bundle-classic
  service: s2
`
	manager := getTestManager(t, conf)
	trigger := plugins.TriggerManual
	_, err := getPluginSet(nil, manager, manager.Config, nil, nil, &trigger)
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	p := manager.Plugin(bundlePlugin.Name)
	if p == nil {
		t.Fatal("Unable to find bundle plugin on manager")
	}
	bp := p.(*bundlePlugin.Plugin)

	// make sure the older style `bundle` config takes precedence
	if bp.Config().Name != "bundle-classic" {
		t.Fatal("Expected bundle plugin config Name to be 'bundle-classic'")
	}

	if len(bp.Config().Bundles) != 1 {
		t.Fatal("Expected a single bundle configured")
	}

	if bp.Config().Bundles["bundle-classic"].Service != "s2" {
		t.Fatalf("Expected the classic bundle to be configured as bundles[0], got: %+v", bp.Config().Bundles)
	}
}

func TestGetPluginSetWithBundlesConfig(t *testing.T) {
	conf := `
services:
  s1:
    url: http://test1.com

bundles:
  bundle-new:
    service: s1
`
	manager := getTestManager(t, conf)
	trigger := plugins.TriggerManual
	_, err := getPluginSet(nil, manager, manager.Config, nil, nil, &trigger)
	if err != nil {
		t.Fatalf("Unexpected error: %s", err)
	}

	p := manager.Plugin(bundlePlugin.Name)
	if p == nil {
		t.Fatal("Unable to find bundle plugin on manager")
	}
	bp := p.(*bundlePlugin.Plugin)

	if len(bp.Config().Bundles) != 1 {
		t.Fatal("Expected a single bundle configured")
	}

	if bp.Config().Bundles["bundle-new"].Service != "s1" {
		t.Fatalf("Expected the bundle to be configured as bundles[0], got: %+v", bp.Config().Bundles)
	}
}

func TestGetPluginSetWithBadManualTriggerBundlesConfig(t *testing.T) {
	confGood := `
services:
  s1:
    url: http://test1.com

bundles:
  bundle-new:
    service: s1
`

	confBad := `
services:
  s1:
    url: http://test1.com

bundles:
  bundle-new:
    service: s1
    trigger: periodic
`

	tests := map[string]struct {
		conf    string
		wantErr bool
		err     error
	}{
		"no_trigger_mode_mismatch": {
			confGood, false, nil,
		},
		"trigger_mode_mismatch": {
			confBad, true, errors.New("invalid configuration for bundle \"bundle-new\": trigger mode mismatch: manual and periodic (hint: check discovery configuration)"),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {

			manager := getTestManager(t, tc.conf)
			trigger := plugins.TriggerManual
			_, err := getPluginSet(nil, manager, manager.Config, nil, nil, &trigger)

			if tc.wantErr {
				if err == nil {
					t.Fatal("Expected error but got nil")
				}

				if tc.err != nil && tc.err.Error() != err.Error() {
					t.Fatalf("Expected error message %v but got %v", tc.err.Error(), err.Error())
				}
			} else if err != nil {
				t.Fatalf("Unexpected error %v", err)
			}
		})
	}
}

func TestGetPluginSetWithBadManualTriggerDecisionLogConfig(t *testing.T) {

	confGood := `
services:
  s1:
    url: http://test1.com

bundles:
  bundle-new:
    service: s1
    trigger: manual
decision_logs:
  service: s1
`

	confBad := `
services:
  s1:
    url: http://test1.com

bundles:
  bundle-new:
    service: s1
    trigger: manual
decision_logs:
  service: s1
  reporting:
    trigger: periodic
`

	tests := map[string]struct {
		conf    string
		wantErr bool
		err     error
	}{
		"no_trigger_mode_mismatch": {
			confGood, false, nil,
		},
		"trigger_mode_mismatch": {
			confBad, true, errors.New("invalid decision_log config: trigger mode mismatch: manual and periodic (hint: check discovery configuration)"),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {

			manager := getTestManager(t, tc.conf)
			trigger := plugins.TriggerManual
			_, err := getPluginSet(nil, manager, manager.Config, nil, nil, &trigger)

			if tc.wantErr {
				if err == nil {
					t.Fatal("Expected error but got nil")
				}

				if tc.err != nil && tc.err.Error() != err.Error() {
					t.Fatalf("Expected error message %v but got %v", tc.err.Error(), err.Error())
				}
			} else if err != nil {
				t.Fatalf("Unexpected error %v", err)
			}
		})
	}
}

func TestGetPluginSetWithBadManualTriggerStatusConfig(t *testing.T) {
	confGood := `
services:
  s1:
    url: http://test1.com

bundles:
  bundle-new:
    service: s1
    trigger: manual
decision_logs:
  service: s1
  reporting:
    trigger: manual
status:
  service: s1
`

	confBad := `
services:
  s1:
    url: http://test1.com

bundles:
  bundle-new:
    service: s1
    trigger: manual
decision_logs:
  service: s1
  reporting:
    trigger: manual
status:
  service: s1
  trigger: periodic
`

	tests := map[string]struct {
		conf    string
		wantErr bool
		err     error
	}{
		"no_trigger_mode_mismatch": {
			confGood, false, nil,
		},
		"trigger_mode_mismatch": {
			confBad, true, errors.New("invalid status config: trigger mode mismatch: manual and periodic (hint: check discovery configuration)"),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {

			manager := getTestManager(t, tc.conf)
			trigger := plugins.TriggerManual
			_, err := getPluginSet(nil, manager, manager.Config, nil, nil, &trigger)

			if tc.wantErr {
				if err == nil {
					t.Fatal("Expected error but got nil")
				}

				if tc.err != nil && tc.err.Error() != err.Error() {
					t.Fatalf("Expected error message %v but got %v", tc.err.Error(), err.Error())
				}
			} else if err != nil {
				t.Fatalf("Unexpected error %v", err)
			}
		})
	}
}

func TestInterQueryBuiltinCacheConfigUpdate(t *testing.T) {
	var config1 *cache.Config
	var config2 *cache.Config
	manager, err := plugins.New([]byte(`{
		"discovery": {"name": "config"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
  }`), "test-id", inmem.New())
	manager.RegisterCacheTrigger(func(c *cache.Config) {
		if config1 == nil {
			config1 = c
		} else if config2 == nil {
			config2 = c
		} else {
			t.Fatal("Expected cache trigger to only be called twice")
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	testPlugin := &reconfigureTestPlugin{counts: map[string]int{}}
	testFactory := testFactory{p: testPlugin}

	disco, err := New(manager, Factories(map[string]plugins.Factory{"test_plugin": testFactory}))
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	initialBundle := makeDataBundle(1, `{
    "config": {
      "caching": {
        "inter_query_builtin_cache": {
          "max_size_bytes": 100
        }
      }
    }
  }`)

	disco.oneShot(ctx, download.Update{Bundle: initialBundle})

	// Verify interQueryBuiltinCacheConfig is triggered with initial config
	if config1 == nil || *config1.InterQueryBuiltinCache.MaxSizeBytes != int64(100) {
		t.Fatalf("Expected cache max size bytes to be 100 after initial discovery, got: %v", config1.InterQueryBuiltinCache.MaxSizeBytes)
	}

	// Verify interQueryBuiltinCache is reconfigured
	updatedBundle := makeDataBundle(2, `{
    "config": {
      "caching": {
        "inter_query_builtin_cache": {
          "max_size_bytes": 200
        }
      }
    }
  }`)

	disco.oneShot(ctx, download.Update{Bundle: updatedBundle})

	if config2 == nil || *config2.InterQueryBuiltinCache.MaxSizeBytes != int64(200) {
		t.Fatalf("Expected cache max size bytes to be 200 after discovery reconfigure, got: %v", config2.InterQueryBuiltinCache.MaxSizeBytes)
	}
}

func TestNDBuiltinCacheConfigUpdate(t *testing.T) {
	type exampleConfig struct {
		v bool
	}
	var config1 *exampleConfig
	var config2 *exampleConfig
	manager, err := plugins.New([]byte(`{
		"discovery": {"name": "config"},
		"services": {
			"localhost": {
				"url": "http://localhost:9999"
			}
		},
  }`), "test-id", inmem.New())
	manager.RegisterNDCacheTrigger(func(x bool) {
		if config1 == nil {
			config1 = &exampleConfig{v: x}
		} else if config2 == nil {
			config2 = &exampleConfig{v: x}
		} else {
			t.Fatal("Expected cache trigger to only be called twice")
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	testPlugin := &reconfigureTestPlugin{counts: map[string]int{}}
	testFactory := testFactory{p: testPlugin}

	disco, err := New(manager, Factories(map[string]plugins.Factory{"test_plugin": testFactory}))
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	initialBundle := makeDataBundle(1, `{
		"config": {
			"nd_builtin_cache": true
		}
	}`)

	disco.oneShot(ctx, download.Update{Bundle: initialBundle})

	// Verify NDBuiltinCache is triggered with initial config
	if config1 == nil || config1.v != true {
		t.Fatalf("Expected ND builtin cache to be enabled after initial discovery, got: %v", config1.v)
	}

	// Verify NDBuiltinCache is reconfigured
	updatedBundle := makeDataBundle(2, `{
		"config": {
			"nd_builtin_cache": false
		}
	}`)

	disco.oneShot(ctx, download.Update{Bundle: updatedBundle})

	if config2 == nil || config2.v != false {
		t.Fatalf("Expected ND builtin cache to be disabled after discovery reconfigure, got: %v", config2.v)
	}
}

func TestPluginManualTriggerLifecycle(t *testing.T) {
	ctx := context.Background()
	m := metrics.New()

	fixture := newTestFixture(t)
	defer fixture.stop()

	// run query
	result, err := fixture.runQuery(ctx, "data.foo.bar", m)
	if err != nil {
		t.Fatal(err)
	}

	if result != nil {
		t.Fatalf("Expected nil result but got %v", result)
	}

	// log result (there should not be a decision log plugin on the manager yet)
	err = fixture.log(ctx, "data.foo.bar", m, &result)
	if err != nil {
		t.Fatal(err)
	}

	// start the discovery plugin
	if err := fixture.plugin.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// trigger the discovery plugin
	fixture.server.discoConfig = `
		{
			"config": {
				"bundles": {
					"authz": {
						"service": "example",
						"trigger": "manual"
					}
				},
				"status": {"service": "example", "trigger": "manual"},
				"decision_logs": {"service": "example", "reporting": {"trigger": "manual"}}
			}
		}`

	fixture.server.dicsoBundleRev = 1

	trigger := make(chan struct{})
	fixture.discoTrigger <- trigger
	<-trigger

	// check if the discovery, bundle, decision log and status plugin are configured
	expectedNum := 4
	if len(fixture.manager.Plugins()) != expectedNum {
		t.Fatalf("Expected %v configured plugins but got %v", expectedNum, len(fixture.manager.Plugins()))
	}

	// run query (since the bundle plugin is not triggered yet, there should not be any activated bundles)
	result, err = fixture.runQuery(ctx, "data.foo.bar", m)
	if err != nil {
		t.Fatal(err)
	}

	if result != nil {
		t.Fatalf("Expected nil result but got %v", result)
	}

	// log result
	err = fixture.log(ctx, "data.foo.bar", m, &result)
	if err != nil {
		t.Fatal(err)
	}

	// trigger the bundle plugin
	fixture.server.bundleData = map[string]any{
		"foo": map[string]any{
			"bar": "hello",
		},
	}
	fixture.server.bundleRevision = "abc"

	trigger = make(chan struct{})
	fixture.bundleTrigger <- trigger
	<-trigger

	// ensure the bundle was activated
	txn := storage.NewTransactionOrDie(ctx, fixture.manager.Store)
	names, err := bundleApi.ReadBundleNamesFromStore(ctx, fixture.manager.Store, txn)
	if err != nil {
		t.Fatal(err)
	}

	expectedNum = 1
	if len(names) != expectedNum {
		t.Fatalf("Expected %d bundles in store, found %d", expectedNum, len(names))
	}

	// stop the "read" transaction
	fixture.manager.Store.Abort(ctx, txn)

	// run query
	result, err = fixture.runQuery(ctx, "data.foo.bar", m)
	if err != nil {
		t.Fatal(err)
	}

	expected := "hello"
	if result != expected {
		t.Fatalf("Expected result %v but got %v", expected, result)
	}

	// log result
	err = fixture.log(ctx, "data.foo.bar", m, &result)
	if err != nil {
		t.Fatal(err)
	}

	// trigger the decision log plugin
	trigger = make(chan struct{})
	fixture.decisionLogTrigger <- trigger
	<-trigger

	expectedNum = 2
	if len(fixture.server.logEvent) != expectedNum {
		t.Fatalf("Expected %d decision log events, found %d", expectedNum, len(fixture.server.logEvent))
	}

	// verify the result in the last log
	if *fixture.server.logEvent[1].Result != expected {
		t.Fatalf("Expected result %v but got %v", expected, result)
	}

	// trigger the status plugin
	trigger = make(chan struct{})
	fixture.statusTrigger <- trigger
	<-trigger

	expectedNum = 1
	if len(fixture.server.statusEvent) != expectedNum {
		t.Fatalf("Expected %d status updates, found %d", expectedNum, len(fixture.server.statusEvent))
	}

	// update the service bundle and trigger the bundle plugin again
	fixture.testServiceBundleUpdateScenario(ctx, m)

	// reconfigure the service bundle config to go from manual to periodic polling. This should result in an error
	// when the discovery plugin tries to reconfigure the bundle
	fixture.server.discoConfig = `
		{
			"config": {
				"bundles": {
					"authz": {
						"service": "example",
						"trigger": "periodic"
					}
				}
			}
		}`
	fixture.server.dicsoBundleRev = 2

	trigger = make(chan struct{})
	fixture.discoTrigger <- trigger
	<-trigger

	// trigger the status plugin
	trigger = make(chan struct{})
	fixture.statusTrigger <- trigger
	<-trigger

	expectedNum = 3
	if len(fixture.server.statusEvent) != expectedNum {
		t.Fatalf("Expected %d status updates, found %d", expectedNum, len(fixture.server.statusEvent))
	}

	// check for error in the last update corresponding to the bad service bundle config
	disco, _ := fixture.server.statusEvent[2].(map[string]any)
	errMsg := disco["discovery"].(map[string]any)["message"]

	expErrMsg := "invalid configuration for bundle \"authz\": trigger mode mismatch: manual and periodic (hint: check discovery configuration)"
	if errMsg != expErrMsg {
		t.Fatalf("Expected error %v but got %v", expErrMsg, errMsg)
	}

	// reconfigure plugins via discovery and then trigger discovery
	fixture.testDiscoReconfigurationScenario(ctx, m)
}

func TestListeners(t *testing.T) {
	manager, err := plugins.New([]byte(`{
			"labels": {"x": "y"},
			"services": {
				"localhost": {
					"url": "http://localhost:9999"
				}
			},
			"discovery": {"name": "config", "persist": true},
		}`), "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	testPlugin := &reconfigureTestPlugin{counts: map[string]int{}}
	testFactory := testFactory{p: testPlugin}

	disco, err := New(manager, Factories(map[string]plugins.Factory{"test_plugin": testFactory}))
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	ensurePluginState(t, disco, plugins.StateNotReady)

	var status *bundlePlugin.Status
	disco.RegisterListener("testlistener", func(s bundlePlugin.Status) {
		status = &s
	})

	// simulate a bundle download error
	disco.oneShot(ctx, download.Update{Error: errors.New("unknown error")})

	if status == nil {
		t.Fatalf("Expected discovery listener to receive status but was nil")
	}

	status = nil
	disco.Unregister("testlistener")

	// simulate a bundle download error
	disco.oneShot(ctx, download.Update{Error: errors.New("unknown error")})
	if status != nil {
		t.Fatalf("Expected discovery listener to be removed but received %v", status)
	}
}

type testFixture struct {
	manager            *plugins.Manager
	plugin             *Discovery
	discoTrigger       chan chan struct{}
	bundleTrigger      chan chan struct{}
	decisionLogTrigger chan chan struct{}
	statusTrigger      chan chan struct{}
	stopCh             chan chan struct{}
	server             *testFixtureServer
}

func newTestFixture(t *testing.T) *testFixture {
	ts := testFixtureServer{
		t:           t,
		statusEvent: []any{},
		logEvent:    []logs.EventV1{},
	}

	ts.start()

	managerConfig := fmt.Appendf(nil, `{
			"labels": {
				"app": "example-app"
			},
            "discovery": {"name": "disco", "trigger": "manual", "decision": "config"},
			"services": [
				{
					"name": "example",
					"url": %q
				}
			]}`, ts.server.URL)

	manager, err := plugins.New(managerConfig, "test-id", inmem.New())
	if err != nil {
		t.Fatal(err)
	}

	disco, err := New(manager)
	if err != nil {
		t.Fatal(err)
	}

	manager.Register(Name, disco)

	tf := testFixture{
		manager:            manager,
		plugin:             disco,
		server:             &ts,
		discoTrigger:       make(chan chan struct{}),
		bundleTrigger:      make(chan chan struct{}),
		decisionLogTrigger: make(chan chan struct{}),
		statusTrigger:      make(chan chan struct{}),
		stopCh:             make(chan chan struct{}),
	}

	go tf.loop(context.Background())

	return &tf
}

func (t *testFixture) loop(ctx context.Context) {

	for {
		select {
		case stop := <-t.stopCh:
			close(stop)
			return
		case done := <-t.discoTrigger:
			if p, ok := t.manager.Plugin(Name).(plugins.Triggerable); ok {
				_ = p.Trigger(ctx)
			}
			close(done)

		case done := <-t.bundleTrigger:
			if p, ok := t.manager.Plugin(bundlePlugin.Name).(plugins.Triggerable); ok {
				_ = p.Trigger(ctx)
			}
			close(done)

		case done := <-t.decisionLogTrigger:
			if p, ok := t.manager.Plugin(logs.Name).(plugins.Triggerable); ok {
				_ = p.Trigger(ctx)
			}
			close(done)
		case done := <-t.statusTrigger:
			if p, ok := t.manager.Plugin(status.Name).(plugins.Triggerable); ok {
				_ = p.Trigger(ctx)
			}
			close(done)
		}
	}
}

func (t *testFixture) runQuery(ctx context.Context, query string, m metrics.Metrics) (any, error) {
	r := rego.New(
		rego.Query(query),
		rego.Store(t.manager.Store),
		rego.Metrics(m),
	)

	// Run evaluation.
	rs, err := r.Eval(ctx)
	if err != nil {
		return nil, err
	}

	if len(rs) == 0 {
		return nil, nil
	}

	return rs[0].Expressions[0].Value, nil
}

func (t *testFixture) log(ctx context.Context, query string, m metrics.Metrics, result *any) error {

	record := server.Info{
		Timestamp: time.Now(),
		Path:      query,
		Metrics:   m,
		Results:   result,
	}

	if logger := logs.Lookup(t.manager); logger != nil {
		if err := logger.Log(ctx, &record); err != nil {
			return fmt.Errorf("decision log: %w", err)
		}
	}
	return nil
}

func (t *testFixture) testServiceBundleUpdateScenario(ctx context.Context, m metrics.Metrics) {
	t.server.bundleData = map[string]any{
		"foo": map[string]any{
			"bar": "world",
		},
	}
	t.server.bundleRevision = "def"

	trigger := make(chan struct{})
	t.bundleTrigger <- trigger
	<-trigger

	// run query
	result, err := t.runQuery(ctx, "data.foo.bar", m)
	if err != nil {
		t.server.t.Fatal(err)
	}

	expected := "world"
	if result != expected {
		t.server.t.Fatalf("Expected result %v but got %v", expected, result)
	}

	// log result
	err = t.log(ctx, "data.foo.bar", m, &result)
	if err != nil {
		t.server.t.Fatal(err)
	}

	// trigger the decision log plugin
	trigger = make(chan struct{})
	t.decisionLogTrigger <- trigger
	<-trigger

	expectedNum := 3
	if len(t.server.logEvent) != expectedNum {
		t.server.t.Fatalf("Expected %d decision log events, found %d", expectedNum, len(t.server.logEvent))
	}

	// verify the result in the last log
	if *t.server.logEvent[2].Result != expected {
		t.server.t.Fatalf("Expected result %v but got %v", expected, result)
	}

	// trigger the status plugin (there should be a pending update corresponding to the last service bundle activation)
	trigger = make(chan struct{})
	t.statusTrigger <- trigger
	<-trigger

	expectedNum = 2
	if len(t.server.statusEvent) != expectedNum {
		t.server.t.Fatalf("Expected %d status updates, found %d", expectedNum, len(t.server.statusEvent))
	}

	// verify the updated bundle revision in the last status update
	bundles, _ := t.server.statusEvent[1].(map[string]any)
	actual := bundles["bundles"].(map[string]any)["authz"].(map[string]any)["active_revision"]

	if actual != t.server.bundleRevision {
		t.server.t.Fatalf("Expected revision %v but got %v", t.server.bundleRevision, actual)
	}
}

func (t *testFixture) testDiscoReconfigurationScenario(ctx context.Context, m metrics.Metrics) {
	t.server.discoConfig = `
		{
			"config": {
				"bundles": {
					"authz": {
						"service": "example",
                        "resource": "newbundles/authz",
						"trigger": "manual"
					}
				},
				"status": {"service": "example", "trigger": "manual", "partition_name": "new"},
				"decision_logs": {"service": "example", "resource": "newlogs", "reporting": {"trigger": "manual"}}
			}
		}`

	t.server.dicsoBundleRev = 3

	trigger := make(chan struct{})
	t.discoTrigger <- trigger
	<-trigger

	// trigger the bundle plugin
	t.server.bundleData = map[string]any{
		"bux": map[string]any{
			"qux": "hello again!",
		},
	}
	t.server.bundleRevision = "ghi"

	trigger = make(chan struct{})
	t.bundleTrigger <- trigger
	<-trigger

	// run query
	result, err := t.runQuery(ctx, "data.bux.qux", m)
	if err != nil {
		t.server.t.Fatal(err)
	}

	expected := "hello again!"
	if result != expected {
		t.server.t.Fatalf("Expected result %v but got %v", expected, result)
	}

	// trigger the status plugin (there should be pending updates corresponding to the last discovery and service bundle activation)
	trigger = make(chan struct{})
	t.statusTrigger <- trigger
	<-trigger

	expectedNum := 4
	if len(t.server.statusEvent) != expectedNum {
		t.server.t.Fatalf("Expected %d status updates, found %d", expectedNum, len(t.server.statusEvent))
	}

	// verify the updated discovery and service bundle revisions in the last status update
	bundles, _ := t.server.statusEvent[3].(map[string]any)
	actual := bundles["bundles"].(map[string]any)["authz"].(map[string]any)["active_revision"]

	if actual != t.server.bundleRevision {
		t.server.t.Fatalf("Expected revision %v but got %v", t.server.bundleRevision, actual)
	}

	disco, _ := t.server.statusEvent[3].(map[string]any)
	actual = disco["discovery"].(map[string]any)["active_revision"]

	expectedRev := fmt.Sprintf("test-revision-%v", t.server.dicsoBundleRev)
	if actual != expectedRev {
		t.server.t.Fatalf("Expected discovery bundle revision %v but got %v", expectedRev, actual)
	}
}

func (t *testFixture) stop() {
	done := make(chan struct{})
	t.stopCh <- done
	<-done

	t.server.stop()
}

type testFixtureServer struct {
	t              *testing.T
	server         *httptest.Server
	discoConfig    string
	dicsoBundleRev int
	bundleData     map[string]any
	bundleRevision string
	statusEvent    []any
	logEvent       []logs.EventV1
}

func (t *testFixtureServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/bundles/disco" {
		// prepare a discovery bundle with some configured plugins

		b := makeDataBundle(t.dicsoBundleRev, t.discoConfig)

		err := bundleApi.NewWriter(w).Write(*b)
		if err != nil {
			t.t.Fatal(err)
		}
	} else if r.URL.Path == "/bundles/authz" || r.URL.Path == "/newbundles/authz" {
		// prepare a regular bundle

		b := bundleApi.Bundle{
			Data:     t.bundleData,
			Manifest: bundleApi.Manifest{Revision: t.bundleRevision},
		}

		err := bundleApi.NewWriter(w).Write(b)
		if err != nil {
			t.t.Fatal(err)
		}
	} else if r.URL.Path == "/status" || r.URL.Path == "/status/new" {

		var event any

		if err := util.NewJSONDecoder(r.Body).Decode(&event); err != nil {
			t.t.Fatal(err)
		}

		t.statusEvent = append(t.statusEvent, event)

	} else if r.URL.Path == "/logs" || r.URL.Path == "/newlogs" {
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.t.Fatal(err)
		}
		var events []logs.EventV1
		if err := json.NewDecoder(gr).Decode(&events); err != nil {
			t.t.Fatal(err)
		}
		if err := gr.Close(); err != nil {
			t.t.Fatal(err)
		}

		t.logEvent = append(t.logEvent, events...)

	} else {
		t.t.Fatalf("unknown path %v", r.URL.Path)
	}

}

func (t *testFixtureServer) start() {
	t.server = httptest.NewServer(http.HandlerFunc(t.handle))
}

func (t *testFixtureServer) stop() {
	t.server.Close()
}

func ensurePluginState(t *testing.T, d *Discovery, state plugins.State) {
	t.Helper()
	status, ok := d.manager.PluginStatus()[Name]
	if !ok {
		t.Fatalf("Expected to find state for %s, found nil", Name)
		return
	}
	if status.State != state {
		t.Fatalf("Unexpected status state found in plugin manager for %s:\n\n\tFound:%+v\n\n\tExpected: %s", Name, status.State, state)
	}
}
