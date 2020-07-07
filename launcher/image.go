// SPDX-License-Identifier: Apache-2.0
//
// Copyright 2019 Renesas Inc.
// Copyright 2019 EPAM Systems Inc.
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

package launcher

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	log "github.com/sirupsen/logrus"

	amqp "aos_servicemanager/amqphandler"
	"aos_servicemanager/utils"
)

/*******************************************************************************
 * Consts
 ******************************************************************************/

const manifestFileName = "manifest.json"
const decryptDir = "/tmp/decrypt"

/*******************************************************************************
 * Type
 ******************************************************************************/

type imageHandler struct {
}

type imageParts struct {
	imageConfigPath    string
	aosSrvConfigPath   string
	serviceFSLayerPath string
	layersDigest       []string
}

/*******************************************************************************
 * Private
 ******************************************************************************/

func (handler *imageHandler) downloadService(serviceInfo serviceInfoToInstall,
	crypt utils.FcryptInterface) (outputFile string, err error) {
	decryptData := amqp.DecryptDataStruct{URLs: serviceInfo.serviceDetails.URLs,
		Sha256:         serviceInfo.serviceDetails.Sha256,
		Sha512:         serviceInfo.serviceDetails.Sha512,
		Size:           serviceInfo.serviceDetails.Size,
		DecryptionInfo: serviceInfo.serviceDetails.DecryptionInfo,
		Signs:          serviceInfo.serviceDetails.Signs}

	destDir, err := ioutil.TempDir("", "aos_")
	if err != nil {
		log.Error("Can't create tmp dir : ", err)
		return outputFile, err
	}
	defer os.RemoveAll(destDir)

	fileName, err := utils.DownloadImage(decryptData, destDir)
	if err != nil {
		log.Error("Can't download package: ", err)
		return outputFile, err
	}
	defer os.RemoveAll(fileName)

	if err = utils.CheckFile(fileName, decryptData); err != nil {
		err = fmt.Errorf("check service checksums error: %s", err.Error())
		return outputFile, err
	}

	if err := os.MkdirAll(decryptDir, 0755); err != nil {
		log.Error("Can't create tmp dir : ", err)
		return outputFile, err
	}

	outputFile = path.Join(decryptDir, filepath.Base(fileName))
	if err = utils.DecryptImage(fileName, outputFile, crypt, decryptData.DecryptionInfo); err != nil {
		err = fmt.Errorf("decrypt service error: %s", err.Error())
		return outputFile, err
	}

	if err = utils.CheckSigns(outputFile, crypt, decryptData.Signs, serviceInfo.chains, serviceInfo.certs); err != nil {
		err = fmt.Errorf("check service signature error: %s", err.Error())
		return outputFile, err
	}

	log.WithField("filename", outputFile).Debug("Decrypt image")

	return outputFile, nil
}

func downloadAndUnpackImage(downloader downloader, serviceInfo serviceInfoToInstall,
	installDir string, crypt utils.FcryptInterface) (err error) {
	// download image
	image, err := downloader.downloadService(serviceInfo, crypt)
	if image != "" {
		defer os.Remove(image)
	}
	if err != nil {
		return err
	}

	// unpack image there
	if err = utils.UnpackTarImage(image, installDir); err != nil {
		return err
	}

	return nil
}

func validateUnpackedImage(installDir string) (err error) {
	manifest, err := getImageManifest(installDir)
	if err != nil {
		return err
	}

	// validate image config
	if err = validateDigest(installDir, manifest.Config.Digest); err != nil {
		return err
	}

	// validate aos service config
	if manifest.AosService != nil {
		if err = validateDigest(installDir, manifest.AosService.Digest); err != nil {
			return err
		}
	}

	layersSize := len(manifest.Layers)
	if layersSize == 0 {
		return errors.New("no layers in image")
	}

	// validate service rootfs layer
	if err = validateDigest(installDir, manifest.Layers[0].Digest); err != nil {
		return err
	}

	return nil
}

func getImageManifest(installDir string) (manifest *serviceManifest, err error) {
	manifestJSON, err := ioutil.ReadFile(path.Join(installDir, manifestFileName))
	if err != nil {
		return nil, err
	}

	manifest = new(serviceManifest)
	if err = json.Unmarshal(manifestJSON, manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

func validateDigest(installDir string, digest digest.Digest) (err error) {
	if err = digest.Validate(); err != nil {
		return err
	}

	file, err := os.Open(path.Join(installDir, "blobs", string(digest.Algorithm()), digest.Hex()))
	if err != nil {
		return err
	}
	defer file.Close()

	buffer := make([]byte, 1024*1024)
	verifier := digest.Verifier()

	for {
		count, readErr := file.Read(buffer)
		if readErr != nil && readErr != io.EOF {
			return err
		}

		if _, err := verifier.Write(buffer[:count]); err != nil {
			return err
		}

		if readErr != nil {
			break
		}
	}

	if verifier.Verified() == false {
		return errors.New("hash missmach")
	}

	return nil
}

func packImage(source, name string) (err error) {
	log.WithFields(log.Fields{"source": source, "name": name}).Debug("Pack image")

	_, err = os.Stat(source)
	if err != nil {
		return err
	}

	writer, err := os.Create(name)
	if err != nil {
		return err
	}

	gzWriter := gzip.NewWriter(writer)
	defer gzWriter.Close()

	tarWriter := tar.NewWriter(gzWriter)
	defer tarWriter.Close()

	return filepath.Walk(source, func(fileName string, fileInfo os.FileInfo, err error) error {
		header, err := tar.FileInfoHeader(fileInfo, fileInfo.Name())
		if err != nil {
			return err
		}

		header.Name = strings.TrimPrefix(strings.Replace(fileName, source, "", -1), string(filepath.Separator))

		if header.Name == "" {
			return nil
		}

		if err = tarWriter.WriteHeader(header); err != nil {
			return err
		}

		if !fileInfo.Mode().IsRegular() {
			return nil
		}

		file, err := os.Open(fileName)
		if err != nil {
			return err
		}
		defer file.Close()

		_, err = io.Copy(tarWriter, file)
		return err
	})
}

func getImageParts(installDir string) (parts imageParts, err error) {
	manifest, err := getImageManifest(installDir)
	if err != nil {
		return parts, err
	}

	parts.imageConfigPath = path.Join(installDir, "blobs", string(manifest.Config.Digest.Algorithm()), string(manifest.Config.Digest.Hex()))

	if manifest.AosService != nil {
		parts.aosSrvConfigPath = path.Join(installDir, "blobs", string(manifest.AosService.Digest.Algorithm()), string(manifest.AosService.Digest.Hex()))
	}

	layersSize := len(manifest.Layers)
	if layersSize == 0 {
		return parts, errors.New("no layers in image")
	}

	rootFSDigest := manifest.Layers[0].Digest

	parts.serviceFSLayerPath = path.Join(installDir, "blobs", string(rootFSDigest.Algorithm()), string(rootFSDigest.Hex()))

	manifest.Layers = manifest.Layers[1:]

	for _, layer := range manifest.Layers {
		parts.layersDigest = append(parts.layersDigest, string(layer.Digest))
	}

	return parts, nil
}
