package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"fmt"
	"github.com/bradleyfalzon/ghinstallation"
	"github.com/google/go-github/v33/github"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const GITHUB_APP_ID = 105923
const GITHUB_INSTALLATION_ID = 15502402

const OWNER = "AxioDL"
const REPO = "metaforce"

func main() {
	ex, err := os.Executable()
	if err != nil {
		panic(err)
	}
	found := false
	pkeyPath := path.Join(filepath.Dir(ex), "pkey.pem")
	if found, err = exists(pkeyPath); err != nil {
		panic(err)
	}
	if !found {
		pkeyPath = "pkey.pem"
	}

	itr, err := ghinstallation.NewKeyFromFile(http.DefaultTransport, GITHUB_APP_ID, GITHUB_INSTALLATION_ID, pkeyPath)
	if err != nil {
		panic(err)
	}

	client := github.NewClient(&http.Client{Transport: itr})
	artifacts, _, err := client.Actions.ListArtifacts(context.Background(), OWNER, REPO, &github.ListOptions{})
	if err != nil {
		panic(err)
	}

	var platformCompilerMap = map[string]string{
		"linux": "clang",
		"macos": "appleclang",
		"win32": "msvc",
	}
	var platformIndex = map[string][]string{
		"linux": {},
		"macos": {},
		"win32": {},
	}

	for _, artifact := range artifacts.Artifacts {
		info := parseArtifactName(*artifact.Name)
		if info.project != "metaforce" || platformCompilerMap[info.platform] != info.compiler {
			continue
		}
		fmt.Println("Selected artifact", info.project, info.version, info.platform, info.compiler, info.arch)
		baseDir := fmt.Sprintf("continuous/%s", info.platform)
		name := fmt.Sprintf("%s-%s-%s-%s", info.project, info.version, info.platform, info.arch)

		extension := ""
		if info.platform == "win32" {
			extension = "zip"
		} else if info.platform == "linux" {
			extension = "tar"
		} else if info.platform == "macos" {
			extension = "dmg"
		}

		// Check if we've previously looked at this artifact and
		// it didn't contain the binaries we wanted
		skipFile := fmt.Sprintf("%s/.skip.%s.%s", baseDir, name, extension)
		if found, err := exists(skipFile); found || err != nil {
			if err != nil {
				panic(err)
			}
			fmt.Println("Skipping bad file", name)
			continue
		}

		// Add to platform index file
		platformIndex[info.platform] = append(platformIndex[info.platform], fmt.Sprintf("%s.%s", name, extension))

		// Check if artifact already exists in output
		outPath := fmt.Sprintf("%s/%s.%s", baseDir, name, extension)
		if exist, err := exists(outPath); exist || err != nil {
			if err != nil {
				panic(err)
			}
			fmt.Println("Skipping existing file", name)
			continue
		}

		zr, err := openArtifact(client, *artifact.ID)
		if err != nil {
			panic(err)
		}

		found := false
		if info.platform == "linux" {
			found, err = writeLinuxTar(zr, name, baseDir)
		} else if info.platform == "win32" {
			found, err = writeWin32Zip(zr, name, baseDir)
		} else if info.platform == "macos" {
			found, err = writeMacosDmg(zr, name, baseDir)
		}
		if err != nil {
			panic(err)
		}

		// If the artifact didn't contain the information we wanted,
		// make sure we skip it in the future
		if !found {
			fmt.Println("Artifact skipped")

			// Remove from platform index
			platformIndex[info.platform] = platformIndex[info.platform][:len(platformIndex[info.platform])-1]

			// Create .skip file
			file, err := os.Create(skipFile)
			if err != nil {
				panic(err)
			}
			err = file.Close()
			if err != nil {
				panic(err)
			}
		}
	}

	for platform, names := range platformIndex {
		file, err := createTempFile(path.Join("continuous", platform))
		if err != nil {
			panic(err)
		}
		if _, err := file.WriteString(strings.Join(names, "\n")); err != nil {
			panic(err)
		}
		if err := file.Close(); err != nil {
			panic(err)
		}
		if err := finalizeTempFile(file.Name(), path.Join("continuous", platform, "index.txt")); err != nil {
			panic(err)
		}
	}
}

func exists(name string) (bool, error) {
	_, err := os.Stat(name)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func openArtifact(client *github.Client, artifactID int64) (*zip.Reader, error) {
	url, _, err := client.Actions.DownloadArtifact(context.Background(), OWNER, REPO, artifactID, true)
	if err != nil {
		return nil, err
	}
	req, err := http.Get(url.String())
	if err != nil {
		return nil, err
	}
	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	return zip.NewReader(bytes.NewReader(body), int64(len(body)))
}

func createTempFile(dir string) (*os.File, error) {
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		return nil, err
	}
	return ioutil.TempFile(dir, ".artifact")
}

func finalizeTempFile(from string, to string) error {
	if err := os.Chmod(from, 0644); err != nil {
		return err
	}
	return os.Rename(from, to)
}

func writeLinuxTar(zr *zip.Reader, name string, baseDir string) (bool, error) {
	foundFile := false
	for _, file := range zr.File {
		if strings.HasSuffix(file.Name, ".AppImage") {
			foundFile = true
			break
		}
	}

	if foundFile {
		of, err := createTempFile(baseDir)
		if err != nil {
			return true, err
		}

		tw := tar.NewWriter(of)
		for _, file := range zr.File {
			// Extract debug file
			if strings.HasPrefix(file.Name, "debug.") {
				dof, err := createTempFile(baseDir)
				if err != nil {
					return true, err
				}
				if err := extractFile(file, dof); err != nil {
					return true, err
				}
				if err := dof.Close(); err != nil {
					return true, err
				}
				if err := finalizeTempFile(dof.Name(), fmt.Sprintf("%s/%s-debug.%s", baseDir, name, strings.Replace(file.Name, "debug.", "", 1))); err != nil {
					return true, err
				}
				continue
			}

			if !strings.HasSuffix(file.Name, ".AppImage") {
				continue
			}

			hdr := tar.Header{
				Name:    fmt.Sprintf("%s.AppImage", name),
				Size:    int64(file.UncompressedSize64),
				Mode:    0777,
				ModTime: file.Modified,
			}
			if err := tw.WriteHeader(&hdr); err != nil {
				return true, err
			}
			if err := extractFile(file, tw); err != nil {
				return true, err
			}
			break
		}

		if err := tw.Close(); err != nil {
			return true, err
		}
		if err := of.Close(); err != nil {
			return true, err
		}
		if err := finalizeTempFile(of.Name(), fmt.Sprintf("%s/%s.tar", baseDir, name)); err != nil {
			return true, err
		}
	}
	return foundFile, nil
}

func writeWin32Zip(zr *zip.Reader, name string, baseDir string) (bool, error) {
	foundFile := false
	for _, file := range zr.File {
		if strings.HasSuffix(file.Name, ".exe") {
			foundFile = true
			break
		}
	}

	if foundFile {
		of, err := createTempFile(baseDir)
		if err != nil {
			return true, err
		}

		zw := zip.NewWriter(of)
		zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
			return flate.NewWriter(out, flate.BestCompression)
		})

		for _, file := range zr.File {
			// Extract debug file
			if strings.HasPrefix(file.Name, "debug.") {
				dof, err := createTempFile(baseDir)
				if err != nil {
					return true, err
				}
				if err := extractFile(file, dof); err != nil {
					return true, err
				}
				if err := dof.Close(); err != nil {
					return true, err
				}
				if err := finalizeTempFile(dof.Name(), fmt.Sprintf("%s/%s-debug.%s", baseDir, name, strings.Replace(file.Name, "debug.", "", 1))); err != nil {
					return true, err
				}
				continue
			}

			if !strings.HasSuffix(file.Name, ".exe") {
				continue
			}

			hdr := zip.FileHeader{
				Name:               file.Name,
				Modified:           file.Modified,
				UncompressedSize64: file.UncompressedSize64,
			}
			w, err := zw.CreateHeader(&hdr)
			if err != nil {
				return true, err
			}
			if err := extractFile(file, w); err != nil {
				return true, err
			}
		}

		if err := zw.Close(); err != nil {
			return true, err
		}
		if err := of.Close(); err != nil {
			return true, err
		}
		if err := finalizeTempFile(of.Name(), fmt.Sprintf("%s/%s.zip", baseDir, name)); err != nil {
			return true, err
		}
	}
	return foundFile, nil
}

func writeMacosDmg(zr *zip.Reader, name string, baseDir string) (bool, error) {
	foundFile := false
	for _, file := range zr.File {
		if strings.HasSuffix(file.Name, ".dmg") {
			foundFile = true
			break
		}
	}

	if foundFile {
		of, err := createTempFile(baseDir)
		if err != nil {
			return true, err
		}

		for _, file := range zr.File {
			// Extract debug file
			if strings.HasPrefix(file.Name, "debug.") {
				dof, err := createTempFile(baseDir)
				if err != nil {
					return true, err
				}
				if err := extractFile(file, dof); err != nil {
					return true, err
				}
				if err := dof.Close(); err != nil {
					return true, err
				}
				if err := finalizeTempFile(dof.Name(), fmt.Sprintf("%s/%s-debug.%s", baseDir, name, strings.Replace(file.Name, "debug.", "", 1))); err != nil {
					return true, err
				}
				continue
			}

			if !strings.HasSuffix(file.Name, ".dmg") {
				continue
			}

			if err := extractFile(file, of); err != nil {
				return true, err
			}
			break
		}

		if err := of.Close(); err != nil {
			return true, err
		}
		if err := finalizeTempFile(of.Name(), fmt.Sprintf("%s/%s.dmg", baseDir, name)); err != nil {
			return true, err
		}
	}
	return foundFile, nil
}

func extractFile(file *zip.File, w io.Writer) error {
	r, err := file.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	if _, err := io.Copy(w, r); err != nil {
		return err
	}
	return nil
}

type artifactInfo struct {
	project  string
	version  string
	platform string
	compiler string
	arch     string
}

func parseArtifactName(name string) artifactInfo {
	info := artifactInfo{}
	split := strings.Split(name, "-")
	if len(split) == 5 {
		// metaforce-123-macos-appleclang-x86_64
		info.project, info.version, info.platform, info.compiler, info.arch = split[0], split[1], split[2], split[3], split[4]
	} else if len(split) == 6 {
		// metaforce-v1.2.3-4-macos-appleclang-x86_64
		info.project, info.version, info.platform, info.compiler, info.arch = split[0], split[1]+"-"+split[2], split[3], split[4], split[5]
	}
	return info
}
