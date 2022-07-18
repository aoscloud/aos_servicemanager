// SPDX-License-Identifier: Apache-2.0
//
// Copyright (C) 2022 Renesas Electronics Corporation.
// Copyright (C) 2022 EPAM Systems, Inc.
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

package whiteouts_test

import (
	"io/fs"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoscloud/aos_common/aoserrors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/aoscloud/aos_servicemanager/utils/whiteouts"
)

/***********************************************************************************************************************
 * Vars
 **********************************************************************************************************************/

var tmpDir string

/***********************************************************************************************************************
 * Init
 **********************************************************************************************************************/

func init() {
	log.SetFormatter(&log.TextFormatter{
		DisableTimestamp: false,
		TimestampFormat:  "2006-01-02 15:04:05.000",
		FullTimestamp:    true,
	})
	log.SetLevel(log.DebugLevel)
	log.SetOutput(os.Stdout)
}

/**********************************************************************************************************************
* Main
**********************************************************************************************************************/

func TestMain(m *testing.M) {
	var err error

	if tmpDir, err = ioutil.TempDir("", "aos_"); err != nil {
		log.Fatalf("Error create tmp folder: %v", err)
	}

	ret := m.Run()

	if err := os.RemoveAll(tmpDir); err != nil {
		log.Errorf("Can't remove tmp folder: %v", err)
	}

	os.Exit(ret)
}

func TestConvertWhiteouts(t *testing.T) {
	layers := filepath.Join(tmpDir, "layers")

	type testWhiteoutsData struct {
		dir         string
		file        string
		modePath    string
		attrDir     string
		mode        fs.FileMode
		compareAttr func(path string) error
	}

	testData := []testWhiteoutsData{
		{
			dir:      filepath.Join(layers, "etc"),
			file:     filepath.Join(layers, "etc", ".wh.test.txt"),
			modePath: filepath.Join(layers, "etc", "test.txt"),
			attrDir:  filepath.Join(layers, "etc"),
			mode:     os.ModeCharDevice,
			compareAttr: func(path string) error {
				opaque := make([]byte, 128)

				if _, err := unix.Lgetxattr(path, "trusted.overlay.opaque", opaque); err == nil {
					return aoserrors.New("Should be error: no extend data available")
				}

				return nil
			},
		},
		{
			dir:      filepath.Join(layers, "bin"),
			file:     filepath.Join(layers, "bin", ".wh..wh..opq"),
			modePath: filepath.Join(layers, "bin"),
			attrDir:  filepath.Join(layers, "bin"),
			mode:     os.ModeDir,
			compareAttr: func(path string) error {
				opaque := make([]byte, 128)

				if _, err := unix.Lgetxattr(path, "trusted.overlay.opaque", opaque); err != nil {
					return aoserrors.Wrap(err)
				}

				if len(opaque) != 1 && opaque[0] != 'y' {
					return aoserrors.New("Unexpected extended attribute")
				}

				return nil
			},
		},
	}

	for _, data := range testData {
		if err := os.MkdirAll(data.dir, 0o755); err != nil {
			t.Fatalf("Can't create dir: %v", err)
		}

		if err := ioutil.WriteFile(data.file, []byte{}, 0o600); err != nil {
			t.Fatalf("Can't create whiteout file: %v", err)
		}
	}

	if err := whiteouts.OCIWhiteoutsToOverlay(layers, 0, 0); err != nil {
		t.Fatalf("Can't convert oci whiteouts to overlay format: %v", err)
	}

	for _, data := range testData {
		fi, err := os.Stat(data.modePath)
		if err != nil {
			t.Errorf("Can't get file info: %v", err)
		}

		if fi.Mode()&data.mode != data.mode {
			t.Error("Unexpected file mode")
		}

		if err := data.compareAttr(data.attrDir); err != nil {
			t.Errorf("Can't compare extended attribute: %v", err)
		}
	}
}
