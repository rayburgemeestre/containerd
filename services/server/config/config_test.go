/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package config

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/containerd/containerd/plugin"
)

func TestMergeConfigs(t *testing.T) {
	a := &Config{
		Version:          2,
		Root:             "old_root",
		RequiredPlugins:  []string{"old_plugin"},
		DisabledPlugins:  []string{"old_plugin"},
		State:            "old_state",
		OOMScore:         1,
		Timeouts:         map[string]string{"a": "1"},
		StreamProcessors: map[string]StreamProcessor{"1": {Path: "2", Returns: "4"}, "2": {Path: "5"}},
	}

	b := &Config{
		Root:             "new_root",
		RequiredPlugins:  []string{"new_plugin1", "new_plugin2"},
		OOMScore:         2,
		Timeouts:         map[string]string{"b": "2"},
		StreamProcessors: map[string]StreamProcessor{"1": {Path: "3"}},
	}

	err := mergeConfig(a, b)
	assert.NoError(t, err)

	assert.Equal(t, 2, a.Version)
	assert.Equal(t, "new_root", a.Root)
	assert.Equal(t, "old_state", a.State)
	assert.Equal(t, 2, a.OOMScore)
	assert.Equal(t, []string{"old_plugin", "new_plugin1", "new_plugin2"}, a.RequiredPlugins)
	assert.Equal(t, []string{"old_plugin"}, a.DisabledPlugins)
	assert.Equal(t, map[string]string{"a": "1", "b": "2"}, a.Timeouts)
	assert.Equal(t, map[string]StreamProcessor{"1": {Path: "3"}, "2": {Path: "5"}}, a.StreamProcessors)
}

func TestResolveImports(t *testing.T) {
	tempDir := t.TempDir()

	for _, filename := range []string{"config_1.toml", "config_2.toml", "test.toml"} {
		err := os.WriteFile(filepath.Join(tempDir, filename), []byte(""), 0600)
		assert.NoError(t, err)
	}

	imports, err := resolveImports(filepath.Join(tempDir, "root.toml"), []string{
		filepath.Join(tempDir, "config_*.toml"), // Glob
		filepath.Join(tempDir, "./test.toml"),   // Path clean up
		"current.toml",                          // Resolve current working dir
	})
	assert.NoError(t, err)

	assert.Equal(t, imports, []string{
		filepath.Join(tempDir, "config_1.toml"),
		filepath.Join(tempDir, "config_2.toml"),
		filepath.Join(tempDir, "test.toml"),
		filepath.Join(tempDir, "current.toml"),
	})
}

func TestLoadSingleConfig(t *testing.T) {
	data := `
version = 2
root = "/var/lib/containerd"

[stream_processors]
  [stream_processors."io.containerd.processor.v1.pigz"]
	accepts = ["application/vnd.docker.image.rootfs.diff.tar.gzip"]
	path = "unpigz"
`
	tempDir := t.TempDir()

	path := filepath.Join(tempDir, "config.toml")
	err := os.WriteFile(path, []byte(data), 0600)
	assert.NoError(t, err)

	var out Config
	err = LoadConfig(path, &out)
	assert.NoError(t, err)
	assert.Equal(t, 2, out.Version)
	assert.Equal(t, "/var/lib/containerd", out.Root)
	assert.Equal(t, map[string]StreamProcessor{
		"io.containerd.processor.v1.pigz": {
			Accepts: []string{"application/vnd.docker.image.rootfs.diff.tar.gzip"},
			Path:    "unpigz",
		},
	}, out.StreamProcessors)
}

func TestLoadConfigWithImports(t *testing.T) {
	data1 := `
version = 2
root = "/var/lib/containerd"
imports = ["data2.toml"]
`

	data2 := `
disabled_plugins = ["io.containerd.v1.xyz"]
`

	tempDir := t.TempDir()

	err := os.WriteFile(filepath.Join(tempDir, "data1.toml"), []byte(data1), 0600)
	assert.NoError(t, err)

	err = os.WriteFile(filepath.Join(tempDir, "data2.toml"), []byte(data2), 0600)
	assert.NoError(t, err)

	var out Config
	err = LoadConfig(filepath.Join(tempDir, "data1.toml"), &out)
	assert.NoError(t, err)

	assert.Equal(t, 2, out.Version)
	assert.Equal(t, "/var/lib/containerd", out.Root)
	assert.Equal(t, []string{"io.containerd.v1.xyz"}, out.DisabledPlugins)
}

func TestLoadConfigWithCircularImports(t *testing.T) {
	data1 := `
version = 2
root = "/var/lib/containerd"
imports = ["data2.toml", "data1.toml"]
`

	data2 := `
disabled_plugins = ["io.containerd.v1.xyz"]
imports = ["data1.toml", "data2.toml"]
`
	tempDir := t.TempDir()

	err := os.WriteFile(filepath.Join(tempDir, "data1.toml"), []byte(data1), 0600)
	assert.NoError(t, err)

	err = os.WriteFile(filepath.Join(tempDir, "data2.toml"), []byte(data2), 0600)
	assert.NoError(t, err)

	var out Config
	err = LoadConfig(filepath.Join(tempDir, "data1.toml"), &out)
	assert.NoError(t, err)

	assert.Equal(t, 2, out.Version)
	assert.Equal(t, "/var/lib/containerd", out.Root)
	assert.Equal(t, []string{"io.containerd.v1.xyz"}, out.DisabledPlugins)

	sort.Strings(out.Imports)
	assert.Equal(t, []string{
		filepath.Join(tempDir, "data1.toml"),
		filepath.Join(tempDir, "data2.toml"),
	}, out.Imports)
}

func TestDecodePlugin(t *testing.T) {
	data := `
version = 2
[plugins."io.containerd.runtime.v1.linux"]
  shim_debug = true
`

	tempDir := t.TempDir()

	path := filepath.Join(tempDir, "config.toml")
	err := os.WriteFile(path, []byte(data), 0600)
	assert.NoError(t, err)

	var out Config
	err = LoadConfig(path, &out)
	assert.NoError(t, err)

	pluginConfig := map[string]interface{}{}
	_, err = out.Decode(&plugin.Registration{Type: "io.containerd.runtime.v1", ID: "linux", Config: &pluginConfig})
	assert.NoError(t, err)
	assert.Equal(t, true, pluginConfig["shim_debug"])
}

// TestDecodePluginInV1Config tests decoding non-versioned
// config (should be parsed as V1 config).
func TestDecodePluginInV1Config(t *testing.T) {
	data := `
[plugins.linux]
  shim_debug = true
`

	path := filepath.Join(t.TempDir(), "config.toml")
	err := os.WriteFile(path, []byte(data), 0600)
	assert.NoError(t, err)

	var out Config
	err = LoadConfig(path, &out)
	assert.NoError(t, err)

	pluginConfig := map[string]interface{}{}
	_, err = out.Decode(&plugin.Registration{ID: "linux", Config: &pluginConfig})
	assert.NoError(t, err)
	assert.Equal(t, true, pluginConfig["shim_debug"])
}

func TestMergingTwoPluginConfigs(t *testing.T) {
	// Configuration that customizes the cni bin_dir
	data1 := `
[plugins."io.containerd.grpc.v1.cri".cni]
    bin_dir = "/cm/local/apps/kubernetes/current/bin/cni"
`
	// Configuration that customizes the registry config_path
	data2 := `
[plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/cm/local/apps/containerd/var/etc/certs.d"
`
	// Write both to disk
	tempDir := t.TempDir()
	err := os.WriteFile(filepath.Join(tempDir, "data1.toml"), []byte(data1), 0600)
	assert.NoError(t, err)
	err = os.WriteFile(filepath.Join(tempDir, "data2.toml"), []byte(data2), 0600)
	assert.NoError(t, err)

	// Parse them both
	var out Config
	var out2 Config
	err = LoadConfig(filepath.Join(tempDir, "data1.toml"), &out)
	assert.NoError(t, err)
	err = LoadConfig(filepath.Join(tempDir, "data2.toml"), &out2)
	assert.NoError(t, err)

	// Merge into one config
	err = mergeConfig(&out, &out2)
	assert.NoError(t, err)

	// Test if all values are present
	cri_plugin := out.Plugins["io.containerd.grpc.v1.cri"]
	assert.Equal(t, cri_plugin.ToMap(), map[string]interface{}{
		// originating from first config
		"cni": map[string]interface{}{
			"bin_dir": "/cm/local/apps/kubernetes/current/bin/cni",
		},
		// originating from second config
		"registry": map[string]interface{}{
			"config_path": "/cm/local/apps/containerd/var/etc/certs.d",
		},
	})
}

func TestMergingTwoPluginConfigsOverwrite(t *testing.T) {
	// Configuration that customizes the cni bin_dir, and registry certs config_path
	data1 := `
[plugins."io.containerd.grpc.v1.cri".cni]
    bin_dir = "/cm/local/apps/kubernetes/current/bin/cni"
`
	// Configuration that customizes the default runtime for containerd
	data2 := `
[plugins."io.containerd.grpc.v1.cri".cni]
    conf_dir = "/tmp"
`
	// Write both to disk
	tempDir := t.TempDir()
	err := os.WriteFile(filepath.Join(tempDir, "data1.toml"), []byte(data1), 0600)
	assert.NoError(t, err)
	err = os.WriteFile(filepath.Join(tempDir, "data2.toml"), []byte(data2), 0600)
	assert.NoError(t, err)

	// Parse them both
	var out Config
	var out2 Config
	err = LoadConfig(filepath.Join(tempDir, "data1.toml"), &out)
	assert.NoError(t, err)
	err = LoadConfig(filepath.Join(tempDir, "data2.toml"), &out2)
	assert.NoError(t, err)

	// Merge into one config
	err = mergeConfig(&out, &out2)
	assert.NoError(t, err)

	// Test if all values are present
	cri_plugin := out.Plugins["io.containerd.grpc.v1.cri"]
	assert.Equal(t, cri_plugin.ToMap(), map[string]interface{}{
		// originating from first config
		"cni": map[string]interface{}{
			"conf_dir": "/tmp",
			// bin_dir is not preserved
		},
	})

	// Restore first config
	err = LoadConfig(filepath.Join(tempDir, "data1.toml"), &out)
	assert.NoError(t, err)

	// Test the other way around
	err = mergeConfig(&out2, &out)
	assert.NoError(t, err)

	// Test if all values are present
	cri_plugin = out.Plugins["io.containerd.grpc.v1.cri"]
	assert.Equal(t, cri_plugin.ToMap(), map[string]interface{}{
		// originating from first config
		"cni": map[string]interface{}{
			"bin_dir": "/cm/local/apps/kubernetes/current/bin/cni",
			// conf_dir is not preserved
		},
	})
}
