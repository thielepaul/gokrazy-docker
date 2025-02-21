package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
)

const dockerUrl = "https://download.docker.com/linux/static/stable/"

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get current working directory: %v", err)
	}

	// check if the binary is called outside of its home directory, so we can ssh and do `docker ps`
	basePath := filepath.Join("/perm/home/" + filepath.Base(os.Args[0]))
	if cwd != basePath {
		clientCmd := exec.Command(filepath.Join(basePath, "docker", "docker"), os.Args[1:]...)
		clientCmd.Stdout = os.Stdout
		clientCmd.Stderr = os.Stderr
		clientCmd.Stdin = os.Stdin
		clientCmd.Run()
		os.Exit(clientCmd.ProcessState.ExitCode())
	}

	version := os.Getenv("DOCKER_VERSION")
	if version == "" {
		version = getLatestVersion()
	}
	log.Println("Desired docker version:", version)

	if !checkVersion(version) {
		downloadDocker(dockerUrl + getArch() + "/docker-" + version + ".tgz")
	}

	log.Println("Starting dockerd")
	cmd := exec.Command("./docker/dockerd", "--data-root=/perm/var/lib/docker", "--ip6tables=false")
	cmd.Env = []string{"PATH=/user:" + filepath.Join(cwd, "docker")}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start dockerd: %v", err)
	}
	log.Println("dockerd started")
}

func getLatestVersion() string {
	log.Println("Getting latest Docker version")
	resp, err := http.Get(dockerUrl + getArch())
	if err != nil {
		log.Fatalf("Failed to get binary index: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read response body: %v", err)
	}
	re := regexp.MustCompile(`docker-([\d.]+)\.tgz`)
	matches := re.FindAllStringSubmatch(string(body), -1)
	if len(matches) == 0 {
		log.Fatalf("No versions found")
	}
	return matches[len(matches)-1][1]
}

func checkVersion(version string) bool {
	versionCmd := exec.Command("./docker/docker", "version", "-f", "json")
	output, err := versionCmd.Output()
	if err != nil {
		log.Println("docker version command failed (as expected):", err)
	}
	var versionResponse struct{ Client struct{ Version string } }
	if err := json.Unmarshal(output, &versionResponse); err != nil {
		log.Println("cannot parse docker version:", err)
		return false
	}
	log.Printf("Docker Client Version: %s\n", versionResponse.Client.Version)
	return versionResponse.Client.Version == version
}

func downloadDocker(url string) {
	log.Println("Downloading Docker:", url)
	if err := os.RemoveAll("docker"); err != nil {
		log.Println("Failed to remove old version:", err)
	}

	resp, err := http.Get(url)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		panic(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			panic(err)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(header.Name, 0755); err != nil {
				panic(err)
			}
		case tar.TypeReg:
			outFile, err := os.Create(header.Name)
			if err == nil {
				err = os.Chmod(header.Name, os.FileMode(header.Mode))
			}
			if err != nil {
				panic(err)
			}
			if _, err := io.Copy(outFile, tr); err != nil {
				panic(err)
			}
			outFile.Close()
		}
	}
}

func getArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "arm":
		return "armhf"
	case "ppc64le", "s390x":
		return runtime.GOARCH
	default:
		log.Fatalf("Unsupported architecture: %s", runtime.GOARCH)
		return ""
	}
}
