package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
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
	// If invoked as "nft" (via symlink or copy in dockerd's PATH), act as the stdin-to-tempfile shim.
	if filepath.Base(os.Args[0]) == "nft" {
		runNftShim()
		return
	}

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

	shimPath := filepath.Join(cwd, "docker", "nft")
	installNftShim(shimPath)

	log.Println("Starting dockerd")
	cmd := exec.Command("./docker/dockerd", "--data-root=/perm/var/lib/docker", "--ip6tables=false", "--firewall-backend=nftables")
	cmd.Env = []string{"PATH=" + filepath.Join(cwd, "docker")}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start dockerd: %v", err)
	}
	log.Println("dockerd started")
}

func installNftShim(shimPath string) {
	_ = os.Remove(shimPath)
	selfExe, err := os.Executable()
	if err != nil {
		log.Fatalf("Failed to resolve executable path for nft shim: %v", err)
	}
	if err := os.Symlink(selfExe, shimPath); err != nil {
		// Fallback to copying the binary if symlinks are not supported on the filesystem
		in, openErr := os.Open(selfExe)
		if openErr != nil {
			log.Fatalf("Failed to open current binary for nft shim: %v", openErr)
		}
		defer in.Close()

		out, createErr := os.OpenFile(shimPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if createErr != nil {
			log.Fatalf("Failed to create nft shim at %s: %v", shimPath, createErr)
		}
		defer out.Close()

		if _, copyErr := io.Copy(out, in); copyErr != nil {
			log.Fatalf("Failed to write nft shim: %v", copyErr)
		}
	}
}

func runNftShim() {
	realNft := "/user/nftables"

	var args []string
	var useStdinFile bool

	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "-f" && i+1 < len(os.Args) && os.Args[i+1] == "-" {
			useStdinFile = true
			i++ // skip both "-f" and "-"
			continue
		}
		args = append(args, os.Args[i])
	}

	if useStdinFile {
		tmp, err := os.CreateTemp("/tmp", "nft-*.rules")
		if err != nil {
			fmt.Fprintf(os.Stderr, "nft shim temp file error: %v\n", err)
			os.Exit(1)
		}
		tmpPath := tmp.Name()
		defer os.Remove(tmpPath)

		if _, err := io.Copy(tmp, os.Stdin); err != nil {
			_ = tmp.Close()
			fmt.Fprintf(os.Stderr, "nft shim stdin copy error: %v\n", err)
			os.Exit(1)
		}
		if err := tmp.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "nft shim close error: %v\n", err)
			os.Exit(1)
		}

		args = append(args, "-f", tmpPath)
	}

	cmd := exec.Command(realNft, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "nft shim exec error: %v\n", err)
		os.Exit(1)
	}
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
