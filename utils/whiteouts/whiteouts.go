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

package whiteouts

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/aoscloud/aos_common/aoserrors"
	"golang.org/x/sys/unix"
)

/***********************************************************************************************************************
* Consts
***********************************************************************************************************************/

const (
	whiteoutPrefix    = ".wh."
	whiteoutOpaqueDir = whiteoutPrefix + whiteoutPrefix + ".opq"
)

/***********************************************************************************************************************
* Public
***********************************************************************************************************************/

func OCIWhiteoutsToOverlay(path string, uid, gid int) error {
	if err := filepath.Walk(path, func(name string, info os.FileInfo, err error) error {
		if err != nil {
			return aoserrors.Wrap(err)
		}

		if info.IsDir() {
			return nil
		}

		base := filepath.Base(name)
		dir := filepath.Dir(name)

		if base == whiteoutOpaqueDir {
			if err := unix.Setxattr(dir, "trusted.overlay.opaque", []byte{'y'}, 0); err != nil {
				return aoserrors.Wrap(err)
			}

			return nil
		}

		if strings.HasPrefix(base, whiteoutPrefix) {
			fullPath := filepath.Join(dir, strings.TrimPrefix(base, whiteoutPrefix))

			if err := unix.Mknod(fullPath, unix.S_IFCHR, 0); err != nil {
				return aoserrors.Wrap(err)
			}
			if err := os.Chown(fullPath, uid, gid); err != nil {
				return aoserrors.Wrap(err)
			}

			return nil
		}

		return nil
	}); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}
