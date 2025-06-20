package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mikerybka/util"
)

func main() {
	token := util.RequireEnvVar("GITHUB_TOKEN")
	webhookURL := util.RequireEnvVar("EXTERNAL_URL")
	port := util.EnvVar("PORT", "2067")
	configFile := filepath.Join(util.HomeDir(), "repos.json")
	config, err := readConfig(configFile)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	for id, repo := range config {
		// Check if folder exists
		name := strings.Split(id, "/")[1]
		path := filepath.Join(util.HomeDir(), name)
		fi, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Println(err)
				return
			} else {
				// If the folder doesn't exist, clone
				gitURL := fmt.Sprintf("https://github.com/%s.git", id)
				err = clone(path, gitURL, repo.Branch)
				if err != nil {
					fmt.Printf("Error cloning %s: %s\n", id, err)
					return
				}
			}
		} else {
			// Error if the namespace is already taken by a file
			if !fi.IsDir() {
				fmt.Println("Error:", path, "is file")
				return
			}

			// Error if path is not a git repo
			branch, err := getBranch(path)
			if err != nil {
				fmt.Println("Error:", err)
				return
			}

			// Error if path is a git repo but checked out to the wrong branch
			if branch != repo.Branch {
				fmt.Println("Error:", repo.ID, "is checked out to the wrong branch")
				return
			}

			// Pull
			err = pull(path)
			if err != nil {
				fmt.Printf("Error pulling %s: %s\n", id, err)
				return
			}
		}

		err = registerHook(token, repo.ID, webhookURL)
		if err != nil {
			fmt.Println("Error:", err)
			return
		}
	}

	// Start webhook handler
	http.HandleFunc("/", webhookHandler)
	err = http.ListenAndServe(":"+port, nil)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
}

func clone(path, gitURL, branch string) error {
	args := []string{"clone"}
	if branch != "" {
		// --single-branch avoids unnecessary history for other branches.
		args = append(args, "--branch", branch, "--single-branch")
	}
	args = append(args, gitURL, path)
	cmd := exec.Command("git", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone failed: %v\n%s", err, out)
	}

	return nil
}

func getBranch(path string) (string, error) {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--abbrev-ref", "HEAD")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", errors.New(stderr.String())
	}
	branch := strings.TrimSpace(stdout.String())
	return branch, nil
}

func pull(path string) error {
	cmd := exec.Command("git", "pull")
	cmd.Dir = path
	b, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, strings.TrimSpace(string(b)))
	}
	return nil
}

func readConfig(path string) (map[string]Repo, error) {
	repos := map[string]Repo{}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	err = json.NewDecoder(f).Decode(&repos)
	if err != nil {
		return nil, err
	}
	return repos, nil
}

type Repo struct {
	ID      string          `json:"id"`
	Branch  string          `json:"branch"`
	Install string          `json:"install"`
	Service *SystemdService `json:"service"`
}

type SystemdService struct {
	Name  string            `json:"name"`
	Env   map[string]string `json:"env"`
	Start string            `json:"start"`
	User  string            `json:"user"`
	Dir   string            `json:"dir"`
}

func registerHook(ghToken, repoID, webhookURL string) error {
	// Get list of current hooks
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/hooks", repoID)
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		panic(err)
	}
	req.Header.Add("Authorization", fmt.Sprintf("token %s", ghToken))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	hooks := []Hook{}
	err = json.NewDecoder(res.Body).Decode(&hooks)
	if err != nil {
		panic(err)
	}

	// Return early if URL is already registered
	for _, hook := range hooks {
		if hook.Config.URL == webhookURL && hook.Active && includes(hook.Events, "push") && hook.Config.ContentType == "json" {
			return nil
		}
	}

	// Create the hook
	body, err := json.Marshal(Hook{
		Name:   "web",
		Active: true,
		Events: []string{"push"},
		Config: &HookConfig{
			URL:         webhookURL,
			ContentType: "json",
		},
	})
	if err != nil {
		panic(err)
	}
	req, err = http.NewRequest("POST", apiURL, bytes.NewReader(body))
	if err != nil {
		panic(err)
	}
	req.Header.Add("Authorization", fmt.Sprintf("token %s", ghToken))
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 201 {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("%d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}

	return nil
}

func includes(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

type Hook struct {
	Name   string      `json:"name"`
	Active bool        `json:"active"`
	Events []string    `json:"events"`
	Config *HookConfig `json:"config"`
}

type HookConfig struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
}

func webhookHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Parse webhook
	req := &WebhookRequest{}
	err := json.NewDecoder(r.Body).Decode(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Read config
	repos, err := readConfig(filepath.Join(util.HomeDir(), "repos.json"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	repoID := req.Repository.FullName
	repo, ok := repos[repoID]
	if !ok {
		http.Error(w, fmt.Sprintf("repo %s not configured", repoID), http.StatusBadRequest)
		return
	}

	// Pull
	path := filepath.Join(util.HomeDir(), req.Repository.Name)
	err = pull(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Stop service
	if repo.Service.Name != "" {
		fmt.Println("systemctl stop", repo.Service.Name)
		cmd := exec.Command("systemctl", "stop", repo.Service.Name)
		cmd.Dir = path
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err = cmd.Run()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Install
	if repo.Install != "" {
		fmt.Println(repo.Install)
		cmd := exec.Command("bash", "-c", repo.Install)
		cmd.Dir = path
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err = cmd.Run()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Reload systemd
	if repo.Service.Name != "" {
		fmt.Println("systemctl daemon-reload")
		cmd := exec.Command("systemctl", "daemon-reload")
		cmd.Dir = path
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err = cmd.Run()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Start service
	if repo.Service.Name != "" {
		fmt.Println("systemctl start", repo.Service.Name)
		cmd := exec.Command("systemctl", "start", repo.Service.Name)
		cmd.Dir = path
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err = cmd.Run()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	fmt.Fprintln(w, "ok in", time.Since(start).Milliseconds(), "ms")
}

type WebhookRequest struct {
	Repository *GithubRepository `json:"repository"`
}

type GithubRepository struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
}
