package runtools

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	COMMAND_TIMEOUT                          = 300
	ANYDBVER_ERROR_NOT_CONFIGURED            = 2
	ANYDBVER_ERROR_BACKEND_PROBLEM           = 3
	ANYDBVER_ANSIBLE_PROBLEM                 = 4
	ANYDBVER_DOCKER_IMAGE_MIXED_WITH_ANSIBLE = 5
	ANYDBVER_UNKNOWN_KEYWORD                 = 6
	ANYDBVER_UNKNOWN_VERSION                 = 7
)

// A long deploy can sit for minutes with no output at all: installing database
// packages is one ansible task that prints nothing until it finishes, and the
// worst measured gap is over five minutes. Without a sign of life that reads as
// a hang, so RunPipe prints a heartbeat whenever a streamed command goes quiet.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// quietHeartbeatInterval is how long a command may print nothing before the
// heartbeat starts, and how often it repeats. ANYDBVER_PROGRESS_INTERVAL
// overrides it in seconds, 0 turns the heartbeat off.
func quietHeartbeatInterval() time.Duration {
	if v := os.Getenv("ANYDBVER_PROGRESS_INTERVAL"); v != "" {
		if seconds, err := strconv.Atoi(v); err == nil {
			return time.Duration(seconds) * time.Second
		}
	}
	return 45 * time.Second
}

func roundDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func lastMeaningfulLine(line string) string {
	line = strings.TrimSpace(ansiEscape.ReplaceAllString(line, ""))
	if len(line) > 90 {
		line = line[:90] + "..."
	}
	return line
}

func HandleDockerProblem(logger *log.Logger, err error) {
	if strings.Contains(err.Error(), "permission denied while trying to connect") {
		logger.Println("The user is not allowed to run docker command, https://docs.docker.com/engine/install/linux-postinstall/")
		os.Exit(ANYDBVER_ERROR_NOT_CONFIGURED)
	}
}

func RunFatal(logger *log.Logger, args []string, errMsg string, ignoreMsg *regexp.Regexp, printCmd bool, env map[string]string) int {
	envVars := make([]string, 0)
	for k, v := range env {
		envVars = append(envVars, k+"="+v)
	}

	if printCmd {
		cmd := strings.Join(append(envVars, args...), " ")
		logger.Println(cmd)
	}

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(envVars, os.Environ()...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		logger.Println(err)
		return 1
	}

	done := make(chan error)
	go func() { done <- cmd.Wait() }()

	select {
	case <-time.After(COMMAND_TIMEOUT * time.Second):
		cmd.Process.Kill()
		logger.Println("Command timed out")
		return 1
	case err := <-done:
		if err != nil {
			if ignoreMsg != nil && ignoreMsg.Match(out.Bytes()) {
				return 1
			}
			logger.Println(out.String())
			logger.Fatalf("%s '%s'", errMsg, strings.Join(args, " "))
			return 1
		}
		return 0
	}
}

func RunPipe(logger *log.Logger, args []string, errMsg string, ignoreMsg *regexp.Regexp, printCmd bool, env map[string]string, timeout int) (string, error) {
	envVars := make([]string, 0)
	for k, v := range env {
		envVars = append(envVars, k+"="+v)
	}

	if printCmd {
		cmd := strings.Join(append(envVars, args...), " ")
		logger.Println(cmd)
	}

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(envVars, os.Environ()...)

	stdoutPipe, _ := cmd.StdoutPipe()
	stderrPipe, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		logger.Println(err)
		return "", err
	}

	full_output := ""
	var wg sync.WaitGroup

	var progress sync.Mutex
	started := time.Now()
	lastOutput := started
	lastLine := ""

	// Function to copy the output from the pipes to the logger
	copyOutput := func(r io.Reader, prefix string) {
		defer wg.Done()
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			output_chunk := scanner.Text()
			full_output = full_output + "\n" + output_chunk
			progress.Lock()
			lastOutput = time.Now()
			if line := lastMeaningfulLine(output_chunk); line != "" {
				lastLine = line
			}
			progress.Unlock()
			logger.Println(prefix, output_chunk)
		}
	}

	wg.Add(2)
	go copyOutput(stdoutPipe, "")
	go copyOutput(stderrPipe, "")

	if interval := quietHeartbeatInterval(); interval > 0 {
		heartbeatDone := make(chan struct{})
		defer close(heartbeatDone)
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-heartbeatDone:
					return
				case <-ticker.C:
					progress.Lock()
					quiet := time.Since(lastOutput)
					last := lastLine
					progress.Unlock()
					if quiet < interval {
						continue
					}
					msg := fmt.Sprintf("... still working: %s elapsed, quiet for %s",
						roundDuration(time.Since(started)), roundDuration(quiet))
					if last != "" {
						msg = msg + ", last step: " + last
					}
					logger.Println(msg)
				}
			}
		}()
	}

	done := make(chan error)
	go func() { done <- cmd.Wait() }()

	select {
	case <-time.After(time.Duration(timeout) * time.Second):
		cmd.Process.Kill()
		wg.Wait()
		logger.Println("Command timed out")
		return full_output, errors.New("Command timed out")
	case err := <-done:
		wg.Wait()
		if err != nil {
			if ignoreMsg != nil && ignoreMsg.Match([]byte(full_output)) {
				return full_output, nil
			}
			logger.Printf("%s '%s'", errMsg, strings.Join(args, " "))
			return full_output, errors.New("not ignoring errors")
		}
		return full_output, nil
	}
}

func RunGetOutput(logger *log.Logger, args []string, errMsg string, ignoreMsg *regexp.Regexp, printCmd bool, env map[string]string, timeout int) (string, error) {
	envVars := make([]string, 0)
	for k, v := range env {
		envVars = append(envVars, k+"="+v)
	}

	if printCmd {
		cmd := strings.Join(append(envVars, args...), " ")
		logger.Println(cmd)
	}

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(envVars, os.Environ()...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		logger.Println(err)
		return out.String(), err
	}

	done := make(chan error)
	go func() { done <- cmd.Wait() }()

	select {
	case <-time.After(time.Duration(timeout) * time.Second):
		cmd.Process.Kill()
		logger.Println("Command timed out")
		return out.String(), errors.New("timeout")
	case err := <-done:
		if err != nil {
			if ignoreMsg != nil && ignoreMsg.Match(out.Bytes()) {
				return out.String(), nil
			}
			return out.String(), errors.New("not ignoring: " + out.String())
		}
		return out.String(), nil
	}
}

func ExecCommandInContainer(logger *log.Logger, full_name string, cmd string, errMsg string) (string, error) {
	cmd_args := []string{"docker", "exec", "-i", full_name, "sh", "-c", cmd}

	env := map[string]string{}
	ignoreMsg := regexp.MustCompile("ignore this")
	return RunPipe(logger, cmd_args, errMsg, ignoreMsg, true, env, COMMAND_TIMEOUT)
}
