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

package sys

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/Microsoft/hcsshim"
	"golang.org/x/sys/windows"
)

const (
	// SddlAdministratorsLocalSystem is local administrators plus NT AUTHORITY\System
	SddlAdministratorsLocalSystem = "D:P(A;OICI;GA;;;BA)(A;OICI;GA;;;SY)"
)

// MkdirAllWithACL is a wrapper for MkdirAll that creates a directory
// ACL'd for Builtin Administrators and Local System.
func MkdirAllWithACL(path string, perm os.FileMode) error {
	return mkdirall(path, true)
}

// MkdirAll implementation that is volume path aware for Windows. It can be used
// as a drop-in replacement for os.MkdirAll()
func MkdirAll(path string, _ os.FileMode) error {
	return mkdirall(path, false)
}

// mkdirall is a custom version of os.MkdirAll modified for use on Windows
// so that it is both volume path aware, and can create a directory with
// a DACL.
func mkdirall(path string, adminAndLocalSystem bool) error {
	if re := regexp.MustCompile(`^\\\\\?\\Volume{[a-z0-9-]+}$`); re.MatchString(path) {
		return nil
	}

	// The rest of this method is largely copied from os.MkdirAll and should be kept
	// as-is to ensure compatibility.

	// Fast path: if we can tell whether path is a directory or file, stop with success or error.
	dir, err := os.Stat(path)
	if err == nil {
		if dir.IsDir() {
			return nil
		}
		return &os.PathError{
			Op:   "mkdir",
			Path: path,
			Err:  syscall.ENOTDIR,
		}
	}

	// Slow path: make sure parent exists and then call Mkdir for path.
	i := len(path)
	for i > 0 && os.IsPathSeparator(path[i-1]) { // Skip trailing path separator.
		i--
	}

	j := i
	for j > 0 && !os.IsPathSeparator(path[j-1]) { // Scan backward over element.
		j--
	}

	if j > 1 {
		// Create parent
		err = mkdirall(path[0:j-1], adminAndLocalSystem)
		if err != nil {
			return err
		}
	}

	// Parent now exists; invoke os.Mkdir or mkdirWithACL and use its result.
	if adminAndLocalSystem {
		err = mkdirWithACL(path)
	} else {
		err = os.Mkdir(path, 0)
	}

	if err != nil {
		// Handle arguments like "foo/." by
		// double-checking that directory doesn't exist.
		dir, err1 := os.Lstat(path)
		if err1 == nil && dir.IsDir() {
			return nil
		}
		return err
	}
	return nil
}

// mkdirWithACL creates a new directory. If there is an error, it will be of
// type *PathError. .
//
// This is a modified and combined version of os.Mkdir and windows.Mkdir
// in golang to cater for creating a directory am ACL permitting full
// access, with inheritance, to any subfolder/file for Built-in Administrators
// and Local System.
func mkdirWithACL(name string) error {
	sa := windows.SecurityAttributes{Length: 0}
	sd, err := windows.SecurityDescriptorFromString(SddlAdministratorsLocalSystem)
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: name, Err: err}
	}
	sa.Length = uint32(unsafe.Sizeof(sa))
	sa.InheritHandle = 1
	sa.SecurityDescriptor = sd

	namep, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: name, Err: err}
	}

	e := windows.CreateDirectory(namep, &sa)
	if e != nil {
		return &os.PathError{Op: "mkdir", Path: name, Err: e}
	}
	return nil
}

// IsAbs is a platform-specific wrapper for filepath.IsAbs. On Windows,
// golang filepath.IsAbs does not consider a path \windows\system32 as absolute
// as it doesn't start with a drive-letter/colon combination. However, in
// docker we need to verify things such as WORKDIR /windows/system32 in
// a Dockerfile (which gets translated to \windows\system32 when being processed
// by the daemon. This SHOULD be treated as absolute from a docker processing
// perspective.
func IsAbs(path string) bool {
	if !filepath.IsAbs(path) {
		if !strings.HasPrefix(path, string(os.PathSeparator)) {
			return false
		}
	}
	return true
}

// ForceRemoveAll is the same as os.RemoveAll, but is aware of io.containerd.snapshotter.v1.windows
// and uses hcsshim to unmount and delete container layers contained therein, in the correct order,
// when passed a containerd root data directory (i.e. the `--root` directory for containerd).
func ForceRemoveAll(path string) error {
	// snapshots/windows/windows.go init()
	const snapshotPlugin = "io.containerd.snapshotter.v1" + "." + "windows"
	// snapshots/windows/windows.go NewSnapshotter()
	snapshotDir := filepath.Join(path, snapshotPlugin, "snapshots")
	if stat, err := os.Stat(snapshotDir); err == nil && stat.IsDir() {
		if err := cleanupWCOWLayers(snapshotDir); err != nil {
			return fmt.Errorf("failed to cleanup WCOW layers in %s: %w", snapshotDir, err)
		}
	}

	return os.RemoveAll(path)
}

func cleanupWCOWLayers(root string) error {
	// See snapshots/windows/windows.go getSnapshotDir()
	var layerNums []int
	var rmLayerNums []int
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if path != root && info.IsDir() {
			name := filepath.Base(path)
			if strings.HasPrefix(name, "rm-") {
				layerNum, err := strconv.Atoi(strings.TrimPrefix(name, "rm-"))
				if err != nil {
					return err
				}
				rmLayerNums = append(rmLayerNums, layerNum)
			} else {
				layerNum, err := strconv.Atoi(name)
				if err != nil {
					return err
				}
				layerNums = append(layerNums, layerNum)
			}
			return filepath.SkipDir
		}

		return nil
	}); err != nil {
		return err
	}

	sort.Sort(sort.Reverse(sort.IntSlice(rmLayerNums)))
	for _, rmLayerNum := range rmLayerNums {
		if err := cleanupWCOWLayer(filepath.Join(root, "rm-"+strconv.Itoa(rmLayerNum))); err != nil {
			return err
		}
	}

	sort.Sort(sort.Reverse(sort.IntSlice(layerNums)))
	for _, layerNum := range layerNums {
		if err := cleanupWCOWLayer(filepath.Join(root, strconv.Itoa(layerNum))); err != nil {
			return err
		}
	}

	return nil
}

func cleanupWCOWLayer(layerPath string) error {
	info := hcsshim.DriverInfo{
		HomeDir: filepath.Dir(layerPath),
	}

	// ERROR_DEV_NOT_EXIST is returned if the layer is not currently prepared or activated.
	// ERROR_FLT_INSTANCE_NOT_FOUND is returned if the layer is currently activated but not prepared.
	if err := hcsshim.UnprepareLayer(info, filepath.Base(layerPath)); err != nil {
		if hcserror, ok := err.(*hcsshim.HcsError); !ok || (hcserror.Err != windows.ERROR_DEV_NOT_EXIST && hcserror.Err != syscall.Errno(windows.ERROR_FLT_INSTANCE_NOT_FOUND)) {
			return fmt.Errorf("failed to unprepare %s: %w", layerPath, err)
		}
	}

	if err := hcsshim.DeactivateLayer(info, filepath.Base(layerPath)); err != nil {
		return fmt.Errorf("failed to deactivate %s: %w", layerPath, err)
	}

	if err := hcsshim.DestroyLayer(info, filepath.Base(layerPath)); err != nil {
		return fmt.Errorf("failed to destroy %s: %w", layerPath, err)
	}

	return nil
}
