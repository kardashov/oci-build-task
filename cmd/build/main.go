package main

import (
	"encoding/json"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/u-root/u-root/pkg/termios"
)

const buildkitExitTimeout = 10 * time.Second

type Config struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	TagFile    string `json:"tag_file"`

	ContextPath    string `json:"context"`
	DockerfilePath string `json:"dockerfile,omitempty"`
	Target         string `json:"target"`
	TargetFile     string `json:"target_file"`

	OutputType string `json:"output_type"`
}

type Request struct {
	ResponsePath string `json:"response_path"`
	Config       Config `json:"config"`
}

type Response struct {
	Outputs []string `json:"outputs"`
}

func main() {
	var req Request
	err := json.NewDecoder(os.Stdin).Decode(&req)
	failIf("read request", err)

	res := Response{
		Outputs: []string{"image", "cache"},
	}

	// limit max columns; Concourse sets a super high value and buildctl happily
	// fills the whole screen with whitespace
	ws, err := termios.GetWinSize(os.Stdout.Fd())
	if err == nil {
		ws.Col = 80

		err = termios.SetWinSize(os.Stdout.Fd(), ws)
		if err != nil {
			logrus.Warn("failed to set window size:", err)
		}
	}

	responseFile, err := os.Create(req.ResponsePath)
	failIf("open response path", err)

	err = os.MkdirAll("image", 0755)
	failIf("create image output folder", err)

	err = os.MkdirAll("cache", 0755)
	failIf("create image output folder", err)

	defer func() {
		err := json.NewEncoder(responseFile).Encode(res)
		failIf("write response", err)

		err = responseFile.Close()
		failIf("close response file", err)
	}()

	cfg := req.Config
	sanitize(&cfg)

	err = run(os.Stdout, "setup-cgroups")
	failIf("setup cgroups", err)

	addr := spawnBuildkitd("/var/log/buildkitd.log")

	buildctlArgs := []string{
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + cfg.ContextPath,
		"--local", "dockerfile=" + cfg.DockerfilePath,
		"--export-cache", "type=local,mode=min,dest=cache",
	}

	if _, err := os.Stat("cache/index.json"); err == nil {
		buildctlArgs = append(buildctlArgs,
			"--import-cache", "type=local,src=cache",
		)
	}

	if cfg.OutputType != "none" {
		buildctlArgs = append(buildctlArgs,
			"--output", "type="+cfg.OutputType+",name="+cfg.Repository+",dest=image/image.tar",
		)
	}

	if cfg.Target != "" {
		buildctlArgs = append(buildctlArgs,
			"--opt target="+cfg.Target,
		)
	}

	err = buildctl(addr, os.Stdout, buildctlArgs...)
	failIf("build", err)
}

func sanitize(cfg *Config) {
	if cfg.Repository == "" {
		logrus.Fatal("repository must be specified")
	}

	if cfg.ContextPath == "" {
		cfg.ContextPath = "."
	}

	if cfg.DockerfilePath == "" {
		cfg.DockerfilePath = cfg.ContextPath
	}

	if cfg.TagFile != "" {
		target, err := ioutil.ReadFile(cfg.TagFile)
		failIf("read target file", err)

		cfg.Tag = strings.TrimSpace(string(target))
	}

	if cfg.TargetFile != "" {
		target, err := ioutil.ReadFile(cfg.TargetFile)
		failIf("read target file", err)

		cfg.Target = strings.TrimSpace(string(target))
	}

	if cfg.OutputType == "" {
		cfg.OutputType = "docker"
	}
}

func buildctl(addr string, out io.Writer, args ...string) error {
	return run(out, "buildctl", append([]string{"--addr=" + addr}, args...)...)
}

func run(out io.Writer, path string, args ...string) error {
	cmd := exec.Command(path, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func spawnBuildkitd(logPath string) string {
	runPath := os.Getenv("XDG_RUNTIME_PATH")
	if runPath == "" {
		runPath = "/run"
	}

	addr := (&url.URL{
		Scheme: "unix",
		Path:   path.Join(runPath, "buildkitd", "buildkitd.sock"),
	}).String()

	buildkitdFlags := []string{"--addr=" + addr}

	var cmd *exec.Cmd
	if os.Getuid() == 0 {
		cmd = exec.Command("buildkitd", buildkitdFlags...)
	} else {
		cmd = exec.Command("rootlesskit", append([]string{"buildkitd"}, buildkitdFlags...)...)
	}

	// kill buildkitd on exit
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGTERM,
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND, 0600)
	failIf("open log file", err)

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	err = cmd.Start()
	failIf("start buildkitd", err)

	err = logFile.Close()
	failIf("close log file", err)

	for {
		err := buildctl(addr, ioutil.Discard, "debug", "workers")
		if err == nil {
			break
		}

		err = cmd.Process.Signal(syscall.Signal(0))
		if err != nil {
			logrus.Warn("builtkitd process probe failed:", err)
			logrus.Info("dumping buildkit logs due to probe failure")

			// fmt.Fprintln(os.Stderr)
			// dumpLogFile(logFile)
			// os.Exit(1)
		}

		logrus.Debugf("waiting for buildkitd...")
		time.Sleep(100 * time.Millisecond)
	}

	logrus.Debug("buildkitd started")

	return addr
}

func dumpLogFile(logFile *os.File) {
	_, err := logFile.Seek(0, 0)
	failIf("seek log file", err)

	_, err = io.Copy(os.Stderr, logFile)
	failIf("copy from log file", err)
}

func failIf(msg string, err error) {
	if err != nil {
		logrus.Fatalln("failed to", msg+":", err)
	}
}